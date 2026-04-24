// Package planner turns a user goal + a snapshot of loaded skills
// into a validated mission graph. Makes one LLM call per uncached
// goal (cache is per-coordinator-session, 5-min TTL, 32-entry LRU,
// single-flight on identical in-flight keys). Never persists —
// persistence is the executor's job downstream.
package planner

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/hugr-lab/hugen/pkg/missions/graph"
	"github.com/hugr-lab/hugen/pkg/models"
	"github.com/hugr-lab/hugen/pkg/skills"
)

const (
	defaultCacheTTL    = 5 * time.Minute
	defaultCacheCap    = 32
	defaultCallTimeout = 30 * time.Second
)

// Planner turns a goal + a set of loaded skills into a validated
// mission graph. Safe for concurrent use across coordinator sessions;
// within one session, concurrent calls for the same (goal, skills)
// pair collapse to a single LLM call.
type Planner struct {
	router       *models.Router
	logger       *slog.Logger
	cache        *planCache
	timeout      time.Duration
	promptHeader string
}

// Options tune the Planner at construction. Zero values map to
// sensible defaults (5-min TTL, 32-entry cap, 30s timeout).
type Options struct {
	CacheTTL    time.Duration
	CacheCap    int
	CallTimeout time.Duration

	// PromptHeader is the operator-editable instruction prose loaded
	// from skills/_coordinator/planner-prompt.md. Empty falls back to
	// defaultPromptHeader so unit tests stay filesystem-free.
	PromptHeader string
}

// New builds a Planner with an internal idempotency cache.
func New(router *models.Router, logger *slog.Logger, opts Options) *Planner {
	if logger == nil {
		logger = slog.Default()
	}
	if opts.CacheTTL <= 0 {
		opts.CacheTTL = defaultCacheTTL
	}
	if opts.CacheCap <= 0 {
		opts.CacheCap = defaultCacheCap
	}
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = defaultCallTimeout
	}
	header := strings.TrimSpace(opts.PromptHeader)
	if header == "" {
		header = defaultPromptHeader
	}
	return &Planner{
		router:       router,
		logger:       logger,
		cache:        newPlanCache(opts.CacheTTL, opts.CacheCap),
		timeout:      opts.CallTimeout,
		promptHeader: header,
	}
}

// Plan resolves `goal` into a validated mission graph. `loaded` is
// the caller's snapshot of the coordinator session's active skills —
// the planner derives its role catalog and the cache digest from
// this list.
//
// Concurrency: same-key calls collapse through the cache's
// single-flight sentinel; different keys run independently.
func (p *Planner) Plan(
	ctx context.Context,
	coordSessionID, goal string,
	loaded []*skills.Skill,
	opts graph.PlanOptions,
) (graph.PlanResult, error) {
	catalog := BuildRoleCatalog(loaded)
	digest := SkillsDigest(loaded)
	key := cacheKey(coordSessionID, goal, digest)

	if !opts.Force {
		if cached, ok := p.cache.get(coordSessionID, key); ok {
			cached.FromCache = true
			return cached, nil
		}
	}

	leader, wait := p.cache.acquire(coordSessionID, key)
	if !leader {
		if cached, ok := wait(ctx); ok {
			cached.FromCache = true
			return cached, nil
		}
	}
	defer p.cache.release(coordSessionID, key)

	plan, err := p.planUncached(ctx, goal, catalog)
	if err != nil {
		p.cache.markFailure(coordSessionID, key)
		return graph.PlanResult{}, err
	}
	p.cache.put(coordSessionID, key, plan)
	return plan, nil
}

// OnCoordinatorClose drops every cache entry scoped to the given
// coordinator session. Call from SessionManager's close hook so a
// restarted conversation never serves a stale DAG.
func (p *Planner) OnCoordinatorClose(coordSessionID string) {
	p.cache.dropSession(coordSessionID)
}

// planUncached does the actual LLM call + validation. Separated so
// tests can drive it without touching the cache.
func (p *Planner) planUncached(
	ctx context.Context,
	goal string,
	catalog []RoleEntry,
) (graph.PlanResult, error) {
	if strings.TrimSpace(goal) == "" {
		return graph.PlanResult{}, fmt.Errorf("missions: planner goal is empty")
	}

	llm := p.router.ModelFor(models.IntentDefault)
	if llm == nil {
		return graph.PlanResult{}, fmt.Errorf("missions: router returned nil model")
	}

	prompt := BuildPrompt(p.promptHeader, catalog, goal)
	req := &model.LLMRequest{
		Contents: []*genai.Content{{
			Role:  "user",
			Parts: []*genai.Part{{Text: prompt}},
		}},
	}

	callCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	var text strings.Builder
	for resp, err := range llm.GenerateContent(callCtx, req, false) {
		if err != nil {
			return graph.PlanResult{}, fmt.Errorf("missions: planner llm: %w", err)
		}
		if resp == nil || resp.Content == nil {
			continue
		}
		for _, part := range resp.Content.Parts {
			if part == nil {
				continue
			}
			if part.Text != "" {
				text.WriteString(part.Text)
			}
		}
		if resp.TurnComplete {
			break
		}
	}

	plan, err := ParseResponse(text.String(), catalog)
	if err != nil {
		return graph.PlanResult{}, err
	}
	return plan, nil
}
