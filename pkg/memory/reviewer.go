package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/hugr-lab/hugen/pkg/models"
	"github.com/hugr-lab/hugen/pkg/scheduler"
	"github.com/hugr-lab/hugen/pkg/skills"
	learnstore "github.com/hugr-lab/hugen/pkg/memory/learning/store"
	memstore "github.com/hugr-lab/hugen/pkg/memory/store"
	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
	"github.com/hugr-lab/query-engine/types"
	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

// Reviewer drives post-session fact extraction. One Reviewer instance
// is shared across the scheduler (scheduler picks session IDs and
// calls Review). Review is idempotent: re-running on a completed
// session is a no-op.
type Reviewer struct {
	memory   *memstore.Client
	learning *learnstore.Client
	sessions *sessstore.Client
	router   *models.Router
	logger   *slog.Logger

	// Injected merged memory config — reviewer needs it to know which
	// categories are valid and what prompt to send. Defaults to
	// nil (agent-level defaults apply). The plumbing from active
	// skills → MergedConfig lives in pkg/session (future task).
	config MergedConfig

	// Volatility-to-duration map from config.MemoryConfig. Falls back
	// to defaults when empty.
	volatility map[string]time.Duration

	// loadSkillMemory is the per-session skill-config fetcher. See
	// ReviewerOptions.LoadSkillMemory.
	loadSkillMemory func(ctx context.Context, skillName string) (*skills.SkillMemoryConfig, error)

	// dedupThreshold is the cosine-distance cutoff at which a newly
	// extracted fact is treated as a reinforcement of an existing one.
	dedupThreshold float64

	// now is injectable for deterministic tests.
	now func() time.Time

	// sched + delay: set by bindScheduler when the reviewer is
	// registered as a scheduler task. QueueReview uses these to nudge
	// the scheduler after ReviewDelay; Tick consumes the pending queue.
	sched *scheduler.Scheduler
	delay time.Duration

	pendingMu sync.Mutex
	pending   []delayedReview
}

// delayedReview is one entry in the in-memory queue: a session ID
// the reviewer will look at after dueAt.
type delayedReview struct {
	sessionID string
	dueAt     time.Time
}

// ReviewerOptions bundle reviewer construction parameters. The
// reviewer builds its own memstore / learnstore / sessstore clients
// internally from Querier + AgentID + AgentShort.
type ReviewerOptions struct {
	Querier    types.Querier
	AgentID    string
	AgentShort string
	Router     *models.Router
	Logger     *slog.Logger
	Config     MergedConfig
	Volatility map[string]time.Duration

	// LoadSkillMemory returns the per-skill memory config for a skill
	// by name. Typically wired to `pkg/skills.Manager.Load(ctx,
	// name).Memory`. When set, Review derives active skills from the
	// transcript's skill_loaded / skill_unloaded events, loads each
	// skill's memory config, and calls Merge. When nil, the reviewer
	// uses its static Config.
	LoadSkillMemory func(ctx context.Context, skillName string) (*skills.SkillMemoryConfig, error)
}

// NewReviewer builds a Reviewer. Router is optional — if nil the
// reviewer refuses to run (returns ErrNoModel). Querier is required;
// the reviewer constructs its own store clients internally.
func NewReviewer(opts ReviewerOptions) (*Reviewer, error) {
	if opts.Querier == nil {
		return nil, fmt.Errorf("learning: Reviewer requires Querier")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	storeOpts := memstore.Options{AgentID: opts.AgentID, AgentShort: opts.AgentShort, Logger: opts.Logger}
	memC, err := memstore.New(opts.Querier, storeOpts)
	if err != nil {
		return nil, fmt.Errorf("learning: build memory store: %w", err)
	}
	learnC, err := learnstore.New(opts.Querier, learnstore.Options{
		AgentID: opts.AgentID, AgentShort: opts.AgentShort, Logger: opts.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("learning: build learning store: %w", err)
	}
	sessC, err := sessstore.New(opts.Querier, sessstore.Options{
		AgentID: opts.AgentID, AgentShort: opts.AgentShort, Logger: opts.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("learning: build sessions store: %w", err)
	}
	return &Reviewer{
		memory:          memC,
		learning:        learnC,
		sessions:        sessC,
		router:          opts.Router,
		logger:          opts.Logger,
		config:          opts.Config,
		volatility:      opts.Volatility,
		loadSkillMemory: opts.LoadSkillMemory,
		dedupThreshold:  0.1, // cosine distance < 0.1 → duplicate (i.e. similarity > 0.9)
		now:             func() time.Time { return time.Now().UTC() },
	}, nil
}

// Review runs the post-session extraction pipeline for the given
// session. Idempotent: if a completed review already exists, returns
// nil.
func (r *Reviewer) Review(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("learning: Reviewer: empty sessionID")
	}
	existing, err := r.learning.GetReview(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("learning: GetReview: %w", err)
	}
	if existing != nil && existing.Status == "completed" {
		return nil // already done
	}
	reviewID := ""
	if existing != nil {
		reviewID = existing.ID
	} else {
		id, err := r.learning.CreateReview(ctx, learnstore.Review{
			SessionID: sessionID, Status: "pending",
		})
		if err != nil {
			return fmt.Errorf("learning: CreateReview: %w", err)
		}
		reviewID = id
	}

	events, err := r.sessions.GetEventsFull(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("learning: GetEventsFull: %w", err)
	}

	// Derive per-session merged config from active skills when a
	// loader was configured. Otherwise fall back to the static
	// agent-level config passed at construction.
	merged := r.mergedConfigFor(ctx, events)

	toolCalls := 0
	for _, ev := range events {
		if ev.EventType == "tool_call" {
			toolCalls++
		}
	}
	minCalls := 1
	if merged.MinToolCalls > 0 {
		minCalls = merged.MinToolCalls
	}
	if toolCalls < minCalls {
		r.logger.Info("reviewer: skipping session below min_tool_calls",
			"session", sessionID, "tool_calls", toolCalls, "min", minCalls)
		return r.learning.CompleteReview(ctx, reviewID, learnstore.ReviewResult{
			ModelUsed: "skipped",
		})
	}

	if r.router == nil {
		_ = r.learning.FailReview(ctx, reviewID, "no router configured")
		return fmt.Errorf("learning: Reviewer: no router")
	}

	notes, _ := r.sessions.ListNotes(ctx, sessionID) // best-effort; notes are optional
	prompt := r.buildPrompt(events, notes, merged)

	llm := r.router.ModelFor(models.IntentSummarization)
	rawOutput, usage, err := RunOnce(ctx, llm, prompt)
	if err != nil {
		_ = r.learning.FailReview(ctx, reviewID, err.Error())
		return fmt.Errorf("learning: LLM: %w", err)
	}

	parsed, err := parseReviewOutput(rawOutput)
	if err != nil {
		_ = r.learning.FailReview(ctx, reviewID, "parse: "+err.Error())
		return fmt.Errorf("learning: parse: %w", err)
	}

	result := learnstore.ReviewResult{
		ModelUsed:  llm.Name(),
		TokensUsed: usage,
	}

	// Store or reinforce each fact.
	for _, f := range parsed.Facts {
		reinforced, err := r.upsertFact(ctx, sessionID, f, merged)
		if err != nil {
			r.logger.Warn("reviewer: upsert fact failed",
				"session", sessionID, "content", f.Content, "err", err)
			continue
		}
		if reinforced {
			result.FactsReinforced++
		} else {
			result.FactsStored++
		}
	}

	for _, hy := range parsed.Hypotheses {
		if _, err := r.learning.CreateHypothesis(ctx, learnstore.Hypothesis{
			Content:        hy.Content,
			Category:       hy.Category,
			Priority:       defaultStr(hy.Priority, "medium"),
			Verification:   hy.Verification,
			EstimatedCalls: hy.EstimatedCalls,
			SourceSession:  sessionID,
		}); err != nil {
			r.logger.Warn("reviewer: create hypothesis failed",
				"session", sessionID, "err", err)
			continue
		}
		result.HypothesesAdded++
	}

	return r.learning.CompleteReview(ctx, reviewID, result)
}

// upsertFact stores a new fact or reinforces a close duplicate.
// Returns reinforced=true when a duplicate was found. Dedup is a
// simple content-substring match until embeddings are wired into the
// reviewer — keeps the path testable without an embedding model.
func (r *Reviewer) upsertFact(ctx context.Context, sessionID string, f extractedFact, merged MergedConfig) (bool, error) {
	// Look for near-duplicates by category + keyword.
	existing, _ := r.memory.Search(ctx, f.Content, nil, memstore.SearchOpts{
		Category: f.Category,
		Limit:    5,
	})
	for _, cand := range existing {
		if equalEnough(cand.Content, f.Content) {
			// Reinforce with modest score bonus.
			bonus := 0.15
			if err := r.memory.Reinforce(ctx, cand.ID, bonus, f.Tags, linkList(f.Links)); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	// Compute validity window from volatility.
	vol := defaultStr(f.Volatility, "stable")
	dur := r.durationFor(vol)
	now := r.now()
	item := memstore.Item{
		Content:    f.Content,
		Category:   f.Category,
		Volatility: vol,
		Score:      r.initialScoreFor(f.Category, merged),
		Source:     "review:" + sessionID,
		ValidFrom:  now,
		ValidTo:    now.Add(dur),
	}
	_, err := r.memory.Store(ctx, item, f.Tags, linkList(f.Links))
	return false, err
}

// buildPrompt assembles the summarisation prompt from transcript +
// notes + merged review prompt. The format is intentionally small —
// the cheap model sees: instruction + transcript window + notes.
func (r *Reviewer) buildPrompt(events []sessstore.EventFull, notes []sessstore.Note, merged MergedConfig) string {
	var sb strings.Builder
	sb.WriteString(r.reviewPrompt(merged))
	sb.WriteString("\n\n## Transcript\n")
	for _, ev := range events {
		switch ev.EventType {
		case "user_message":
			sb.WriteString("USER: ")
			sb.WriteString(ev.Content)
		case "llm_response":
			sb.WriteString("AGENT: ")
			sb.WriteString(ev.Content)
		case "tool_call":
			sb.WriteString("TOOL_CALL: ")
			sb.WriteString(ev.ToolName)
			if len(ev.ToolArgs) > 0 {
				b, _ := json.Marshal(ev.ToolArgs)
				sb.WriteString(" ")
				sb.Write(b)
			}
		case "tool_result":
			sb.WriteString("TOOL_RESULT: ")
			sb.WriteString(ev.ToolName)
			sb.WriteString(" → ")
			sb.WriteString(truncate(ev.ToolResult, 1000))
		default:
			continue
		}
		sb.WriteString("\n")
	}
	if len(notes) > 0 {
		sb.WriteString("\n## Session notes\n")
		for _, n := range notes {
			sb.WriteString("- ")
			sb.WriteString(n.Content)
			sb.WriteString("\n")
		}
	}
	sb.WriteString(`
## Output format
Reply with a single JSON object:
{
  "facts": [{"content": "...", "category": "...", "volatility": "stable|slow|moderate|fast|volatile", "tags": ["..."], "links": ["..."]}],
  "hypotheses": [{"content": "...", "category": "...", "priority": "high|medium|low", "verification": "...", "estimated_calls": 3}]
}`)
	return sb.String()
}

func (r *Reviewer) reviewPrompt(merged MergedConfig) string {
	if merged.ReviewPrompt != "" {
		return merged.ReviewPrompt
	}
	if r.config.ReviewPrompt != "" {
		return r.config.ReviewPrompt
	}
	return `You are reviewing a completed agent session. Extract durable facts worth remembering (schema structures, working query templates, anti-patterns, user preferences) and testable hypotheses you noticed but did not verify. Skip chitchat, retries, and one-off noise.`
}

// mergedConfigFor builds the per-session merged config by replaying
// skill_loaded / skill_unloaded events, then asking loadSkillMemory
// for each active skill's memory.yaml. Falls back to the reviewer's
// static config when no loader is wired.
func (r *Reviewer) mergedConfigFor(ctx context.Context, events []sessstore.EventFull) MergedConfig {
	if r.loadSkillMemory == nil {
		return r.config
	}
	active := map[string]struct{}{}
	for _, ev := range events {
		switch ev.EventType {
		case sessstore.EventTypeSkillLoaded:
			if name := skillNameFromEvent(ev); name != "" {
				active[name] = struct{}{}
			}
		case sessstore.EventTypeSkillUnloaded:
			delete(active, skillNameFromEvent(ev))
		}
	}
	if len(active) == 0 {
		return r.config
	}
	configs := make([]NamedConfig, 0, len(active))
	for name := range active {
		cfg, err := r.loadSkillMemory(ctx, name)
		if err != nil {
			r.logger.Warn("reviewer: load skill memory", "skill", name, "err", err)
			continue
		}
		configs = append(configs, NamedConfig{Name: name, Config: cfg})
	}
	return MergeWithLogger(configs, r.logger)
}

func skillNameFromEvent(ev sessstore.EventFull) string {
	if ev.Metadata != nil {
		if name, ok := ev.Metadata["skill"].(string); ok && name != "" {
			return name
		}
	}
	return ev.Content
}

func (r *Reviewer) durationFor(volatility string) time.Duration {
	if r.volatility != nil {
		if d, ok := r.volatility[volatility]; ok && d > 0 {
			return d
		}
	}
	switch volatility {
	case "volatile":
		return 24 * time.Hour
	case "fast":
		return 7 * 24 * time.Hour
	case "moderate":
		return 30 * 24 * time.Hour
	case "slow":
		return 90 * 24 * time.Hour
	}
	return 365 * 24 * time.Hour // stable default
}

func (r *Reviewer) initialScoreFor(category string, merged MergedConfig) float64 {
	if merged.Categories != nil {
		if cat, ok := merged.Categories[category]; ok && cat.InitialScore > 0 {
			return cat.InitialScore
		}
	}
	if r.config.Categories != nil {
		if cat, ok := r.config.Categories[category]; ok && cat.InitialScore > 0 {
			return cat.InitialScore
		}
	}
	return 0.5
}

// ------------------------------------------------------------
// JSON parsing
// ------------------------------------------------------------

type extractedFact struct {
	Content    string   `json:"content"`
	Category   string   `json:"category"`
	Volatility string   `json:"volatility"`
	Tags       []string `json:"tags"`
	Links      []string `json:"links"`
}

type extractedHypothesis struct {
	Content        string `json:"content"`
	Category       string `json:"category"`
	Priority       string `json:"priority"`
	Verification   string `json:"verification"`
	EstimatedCalls int    `json:"estimated_calls"`
}

type reviewOutput struct {
	Facts      []extractedFact       `json:"facts"`
	Hypotheses []extractedHypothesis `json:"hypotheses"`
}

func parseReviewOutput(raw string) (reviewOutput, error) {
	raw = strings.TrimSpace(raw)
	// Strip markdown code fences if present.
	if strings.HasPrefix(raw, "```") {
		if idx := strings.Index(raw, "\n"); idx >= 0 {
			raw = raw[idx+1:]
		}
		raw = strings.TrimSuffix(strings.TrimSpace(raw), "```")
	}
	// Find the outermost {...} span.
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end < 0 || end <= start {
		return reviewOutput{}, fmt.Errorf("no JSON object in output")
	}
	var out reviewOutput
	if err := json.Unmarshal([]byte(raw[start:end+1]), &out); err != nil {
		return reviewOutput{}, err
	}
	return out, nil
}

// ------------------------------------------------------------
// helpers
// ------------------------------------------------------------

func RunOnce(ctx context.Context, llm model.LLM, prompt string) (string, int, error) {
	req := &model.LLMRequest{
		Contents: []*genai.Content{{
			Role:  "user",
			Parts: []*genai.Part{{Text: prompt}},
		}},
	}
	var out strings.Builder
	var totalTokens int
	var seq iter.Seq2[*model.LLMResponse, error] = llm.GenerateContent(ctx, req, false)
	for resp, err := range seq {
		if err != nil {
			return "", totalTokens, err
		}
		if resp == nil {
			continue
		}
		if resp.Content != nil {
			for _, p := range resp.Content.Parts {
				if p != nil && p.Text != "" {
					out.WriteString(p.Text)
				}
			}
		}
		if resp.UsageMetadata != nil {
			totalTokens = int(resp.UsageMetadata.TotalTokenCount)
		}
		if resp.TurnComplete {
			break
		}
	}
	return out.String(), totalTokens, nil
}

func equalEnough(a, b string) bool {
	// Crude similarity fallback: lowercased exact match or one being a
	// prefix of the other within 20% length tolerance. Replace with
	// embedding cosine ≥ 0.9 once the reviewer uses embeddings.
	aa := strings.ToLower(strings.TrimSpace(a))
	bb := strings.ToLower(strings.TrimSpace(b))
	if aa == bb {
		return true
	}
	if len(aa) == 0 || len(bb) == 0 {
		return false
	}
	if len(aa) < len(bb) {
		aa, bb = bb, aa
	}
	if !strings.HasPrefix(aa, bb) {
		return false
	}
	return float64(len(bb))/float64(len(aa)) >= 0.8
}

func defaultStr(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func linkList(targetIDs []string) []memstore.Link {
	if len(targetIDs) == 0 {
		return nil
	}
	links := make([]memstore.Link, 0, len(targetIDs))
	for _, id := range targetIDs {
		links = append(links, memstore.Link{TargetID: id, Relation: "related"})
	}
	return links
}

// ------------------------------------------------------------
// Scheduler integration
// ------------------------------------------------------------

// reviewTaskName is the name the Reviewer uses when registering its
// Tick with the scheduler. Kept package-private so the string lives
// in one place (bindScheduler callers + Wake callers).
const reviewTaskName = "memory.review"

// bindScheduler attaches the reviewer to a running scheduler so that
// QueueReview can Wake the matching task. Typically called by
// memory.Register; not part of the public Reviewer API.
func (r *Reviewer) bindScheduler(sched *scheduler.Scheduler, delay time.Duration) {
	r.sched = sched
	r.delay = delay
}

// QueueReview implements sessions.ReviewQueuer. Called by
// sessions.Manager.Delete when a session closes. Appends sessionID to
// the in-memory pending queue with a dueAt timestamp, and schedules a
// Wake after the configured delay so the classifier has time to flush
// the closed session's transcript before Review reads it.
//
// Idempotent: multiple QueueReview calls for the same sessionID each
// add a pending entry, but Tick deduplicates by session before
// invoking Review (which is itself idempotent on session_reviews).
func (r *Reviewer) QueueReview(sessionID string) {
	if sessionID == "" {
		return
	}
	dueAt := r.nowFn().Add(r.delay)
	r.pendingMu.Lock()
	r.pending = append(r.pending, delayedReview{sessionID: sessionID, dueAt: dueAt})
	r.pendingMu.Unlock()

	if r.sched == nil {
		return
	}
	if r.delay <= 0 {
		r.sched.Wake(reviewTaskName)
		return
	}
	time.AfterFunc(r.delay, func() { r.sched.Wake(reviewTaskName) })
}

// Tick is the Task registered with scheduler.Every("memory.review",…).
// It drains the in-memory pending queue for entries whose dueAt has
// passed. When pending is empty, it consults the hub for persisted
// session_reviews rows (crash recovery) — one at a time to keep
// user-facing turns unblocked.
func (r *Reviewer) Tick(ctx context.Context) error {
	due := r.dueSessions(r.nowFn())

	if len(due) == 0 {
		pending, err := r.learning.ListPendingReviews(ctx, 1)
		if err != nil {
			return err
		}
		for _, p := range pending {
			due = append(due, p.SessionID)
		}
	}

	for _, sid := range due {
		if err := r.Review(ctx, sid); err != nil {
			r.logger.Warn("memory.review: session review failed",
				"session", sid, "err", err)
		}
	}
	return nil
}

// dueSessions pops due pending entries (dueAt <= now) from the queue.
// Entries whose dueAt is still in the future stay on the queue.
// Duplicate session IDs are collapsed to a single entry.
func (r *Reviewer) dueSessions(now time.Time) []string {
	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()
	if len(r.pending) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(r.pending))
	var due []string
	kept := r.pending[:0]
	for _, p := range r.pending {
		if p.dueAt.After(now) {
			kept = append(kept, p)
			continue
		}
		if _, dup := seen[p.sessionID]; dup {
			continue
		}
		seen[p.sessionID] = struct{}{}
		due = append(due, p.sessionID)
	}
	r.pending = kept
	return due
}

// nowFn returns the reviewer's now() function; falls back to
// time.Now().UTC() when unset so QueueReview is safe to call without
// explicit test wiring.
func (r *Reviewer) nowFn() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now().UTC()
}
