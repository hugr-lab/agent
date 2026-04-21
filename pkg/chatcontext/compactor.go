package chatcontext

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/hugr-lab/hugen/pkg/memory"
	"github.com/hugr-lab/hugen/pkg/models"
	"github.com/hugr-lab/hugen/pkg/skills"
	memdb "github.com/hugr-lab/hugen/pkg/store/memory"
	sessdb "github.com/hugr-lab/hugen/pkg/store/sessions"
	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

// Compactor folds the oldest turn groups of req.Contents into a
// single synthetic summary message when the estimated context usage
// crosses Threshold. Runs as a BeforeModelCallback in the
// llmagent BeforeModelCallbacks chain.
//
// Invariants (ADR 005):
//   - Never splits a tool_call from its matching tool_result.
//   - Does not touch the session's fixed prompt part (constitution +
//     skills + refs + session notes + memory status).
//   - Emits one "compaction" session_events row with metadata
//     describing the fold so the post-session reviewer can still see
//     how many turns were summarised.
type Compactor struct {
	memory          *memdb.Client
	sessions        *sessdb.Client
	router          *models.Router
	tokens          *models.TokenEstimator
	threshold       float64
	minTurns        int
	logger          *slog.Logger
	loadSkillMemory func(ctx context.Context, skillName string) (*skills.SkillMemoryConfig, error)
}

// CompactorOptions bundles compactor construction parameters.
type CompactorOptions struct {
	Memory    *memdb.Client
	Sessions  *sessdb.Client
	Router    *models.Router
	Tokens    *models.TokenEstimator
	Threshold float64 // default 0.70
	MinTurns  int     // minimum turn groups retained after compaction; default 4
	Logger    *slog.Logger

	// LoadSkillMemory returns the per-skill memory config for a skill
	// by name. When set, the compactor uses the session's active
	// skills' compaction hints (preserve / discard). When nil, the
	// summary prompt stays generic.
	LoadSkillMemory func(ctx context.Context, skillName string) (*skills.SkillMemoryConfig, error)
}

// NewCompactor constructs a Compactor.
func NewCompactor(opts CompactorOptions) (*Compactor, error) {
	if opts.Sessions == nil {
		return nil, fmt.Errorf("learning: Compactor requires Sessions")
	}
	if opts.Router == nil {
		return nil, fmt.Errorf("learning: Compactor requires Router")
	}
	if opts.Tokens == nil {
		opts.Tokens = models.NewTokenEstimator()
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Threshold <= 0 || opts.Threshold >= 1 {
		opts.Threshold = 0.70
	}
	if opts.MinTurns <= 0 {
		opts.MinTurns = 4
	}
	return &Compactor{
		memory:          opts.Memory,
		sessions:        opts.Sessions,
		router:          opts.Router,
		tokens:          opts.Tokens,
		threshold:       opts.Threshold,
		minTurns:        opts.MinTurns,
		logger:          opts.Logger,
		loadSkillMemory: opts.LoadSkillMemory,
	}, nil
}

// Before is the ADK BeforeModelCallback. Returns (nil, nil) in the
// steady state so the chain continues to the next callback.
func (c *Compactor) Before(ctx adkagent.CallbackContext, req *model.LLMRequest) (*model.LLMResponse, error) {
	if req == nil || len(req.Contents) == 0 {
		return nil, nil
	}
	// Estimate prompt token load using the calibrated estimator.
	ratio := c.usageRatio(req)
	if ratio < c.threshold {
		return nil, nil
	}
	// Select oldest turn groups to fold. Keep MinTurns recent groups.
	cutoff := len(req.Contents) - c.minTurns
	if cutoff <= 0 {
		return nil, nil
	}
	oldest, tail := c.splitAtSafeBoundary(req.Contents, cutoff)
	if len(oldest) == 0 {
		return nil, nil
	}

	// Pull merged compaction hints for the session's active skills.
	// Falls back to the plain summary prompt when no loader is wired
	// or no skill contributes hints.
	sid := ctx.SessionID()
	merged := c.mergedHints(ctx, sid)
	summary, err := c.summarise(ctx, oldest, merged)
	if err != nil {
		c.logger.Warn("compactor: summarise failed", "err", err)
		return nil, nil // fall through — better to ship the long context than fail the turn
	}

	// Replace oldest with a single synthetic summary message.
	newContents := make([]*genai.Content, 0, 1+len(tail))
	newContents = append(newContents, &genai.Content{
		Role:  "user",
		Parts: []*genai.Part{{Text: "[compacted " + itoa(len(oldest)) + " earlier turns]\n" + summary}},
	})
	newContents = append(newContents, tail...)
	req.Contents = newContents

	if sid != "" && c.sessions != nil {
		_, _ = c.sessions.AppendEvent(ctx, sessdb.Event{
			SessionID: sid,
			EventType: "compaction",
			Author:    "system",
			Content:   summary,
			Metadata: map[string]any{
				"original_turns": len(oldest),
				"summary_tokens": c.tokens.Estimate(summary),
			},
		})
	}
	return nil, nil
}

// splitAtSafeBoundary walks backwards from idx until it lands at a
// boundary that does NOT split a FunctionCall from its matching
// FunctionResponse. Returns (oldest, tail) where oldest ⊕ tail ==
// contents.
func (c *Compactor) splitAtSafeBoundary(contents []*genai.Content, idx int) ([]*genai.Content, []*genai.Content) {
	for idx < len(contents) && c.carriesFunctionResponse(contents[idx]) {
		// A response without its call in `oldest` — move boundary
		// right so the response stays in `tail` next to future
		// turns; the preceding call goes into oldest.
		idx++
	}
	if idx <= 0 {
		return nil, contents
	}
	if idx >= len(contents) {
		return contents, nil
	}
	oldest := make([]*genai.Content, idx)
	copy(oldest, contents[:idx])
	tail := make([]*genai.Content, len(contents)-idx)
	copy(tail, contents[idx:])
	return oldest, tail
}

func (c *Compactor) carriesFunctionResponse(ct *genai.Content) bool {
	if ct == nil {
		return false
	}
	for _, p := range ct.Parts {
		if p != nil && p.FunctionResponse != nil {
			return true
		}
	}
	return false
}

// summarise runs a single cheap-model call with the oldest turn
// groups serialised as plain text. Keeps the prompt small — the
// reviewer gets the full transcript anyway, this is only about
// context relief.
func (c *Compactor) summarise(ctx context.Context, oldest []*genai.Content, merged memory.MergedConfig) (string, error) {
	var b strings.Builder
	b.WriteString("Summarise the following conversation turns into a short paragraph (≤ 150 words). ")
	if len(merged.CompactPreserve) > 0 {
		b.WriteString("Preserve: ")
		b.WriteString(strings.Join(merged.CompactPreserve, ", "))
		b.WriteString(". ")
	} else {
		b.WriteString("Preserve concrete identifiers, schema names, numeric results, and error messages. ")
	}
	if len(merged.CompactDiscard) > 0 {
		b.WriteString("Drop: ")
		b.WriteString(strings.Join(merged.CompactDiscard, ", "))
		b.WriteString(".")
	} else {
		b.WriteString("Drop greetings and retries.")
	}
	b.WriteString("\n\n")
	for _, ct := range oldest {
		if ct == nil {
			continue
		}
		role := ct.Role
		if role == "" {
			role = "user"
		}
		b.WriteString(strings.ToUpper(role))
		b.WriteString(": ")
		for _, p := range ct.Parts {
			if p == nil {
				continue
			}
			if p.Text != "" {
				b.WriteString(p.Text)
			}
			if p.FunctionCall != nil {
				b.WriteString("[tool_call: ")
				b.WriteString(p.FunctionCall.Name)
				b.WriteString("]")
			}
			if p.FunctionResponse != nil {
				b.WriteString("[tool_result: ")
				b.WriteString(p.FunctionResponse.Name)
				b.WriteString("]")
			}
		}
		b.WriteString("\n")
	}
	llm := c.router.ModelFor(models.IntentSummarization)
	out, _, err := memory.RunOnce(ctx, llm, b.String())
	return out, err
}

// usageRatio estimates how close the current req.Contents is to the
// model's context budget. Uses the calibrated models.TokenEstimator on a
// concatenated string view — heuristic, good enough to trigger
// compaction without reaching for a precise tokenizer.
func (c *Compactor) usageRatio(req *model.LLMRequest) float64 {
	var chars int
	for _, ct := range req.Contents {
		if ct == nil {
			continue
		}
		for _, p := range ct.Parts {
			if p == nil {
				continue
			}
			chars += len(p.Text)
			if p.FunctionResponse != nil {
				// Rough approximation: assume JSON payload ~ 2x text
				chars += 2000
			}
		}
	}
	est := c.tokens.Estimate(strings.Repeat("x", chars))
	// Budget: 128k context assumed when not known. Better heuristic
	// would read config; compactor consumers can pass a tighter
	// estimator if they know the model's window.
	const defaultBudget = 128_000
	return float64(est) / float64(defaultBudget)
}

// Callback returns the compactor as a llmagent.BeforeModelCallback
// function value for direct use in llmagent.Config.
func (c *Compactor) Callback() llmagent.BeforeModelCallback {
	return c.Before
}

// mergedHints returns the merged compaction hints (preserve / discard)
// for the session's active skills. When no hub/loader are wired, or
// the session has no transcript, returns a zero-value memory.MergedConfig so
// summarise uses its built-in defaults.
func (c *Compactor) mergedHints(ctx context.Context, sid string) memory.MergedConfig {
	if c.loadSkillMemory == nil || c.sessions == nil || sid == "" {
		return memory.MergedConfig{}
	}
	events, err := c.sessions.GetEvents(ctx, sid)
	if err != nil {
		return memory.MergedConfig{}
	}
	active := map[string]struct{}{}
	for _, ev := range events {
		switch ev.EventType {
		case sessdb.EventTypeSkillLoaded:
			if name := skillNameFromSessionEvent(ev); name != "" {
				active[name] = struct{}{}
			}
		case sessdb.EventTypeSkillUnloaded:
			delete(active, skillNameFromSessionEvent(ev))
		}
	}
	if len(active) == 0 {
		return memory.MergedConfig{}
	}
	configs := make([]memory.NamedConfig, 0, len(active))
	for name := range active {
		cfg, err := c.loadSkillMemory(ctx, name)
		if err != nil {
			continue
		}
		configs = append(configs, memory.NamedConfig{Name: name, Config: cfg})
	}
	return memory.MergeWithLogger(configs, c.logger)
}

// skillNameFromSessionEvent is the SessionEvent counterpart to
// skillNameFromEvent — shares the Metadata["skill"] fallback to
// SessionEvent.Content.
func skillNameFromSessionEvent(ev sessdb.Event) string {
	if ev.Metadata != nil {
		if name, ok := ev.Metadata["skill"].(string); ok && name != "" {
			return name
		}
	}
	return ev.Content
}

func itoa(n int) string {
	// small helper to avoid importing strconv for a single int-to-string.
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
