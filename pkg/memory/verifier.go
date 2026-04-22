package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hugr-lab/hugen/pkg/models"
	learnstore "github.com/hugr-lab/hugen/pkg/memory/learning/store"
	memstore "github.com/hugr-lab/hugen/pkg/memory/store"
	"github.com/hugr-lab/query-engine/types"
)

// Verifier runs one hypothesis check per VerifyNext invocation. The
// scheduler drives this on its 20-priority lane (ADR v7.2 §8). Each
// check is a single cheap-model call asking the model to run a
// bounded verification in-place (no sub-session) — acceptable for
// standalone mode; Hub-mode can later route verification through a
// sub-agent with richer tool access.
type Verifier struct {
	memory   *memstore.Client
	learning *learnstore.Client
	router   *models.Router
	logger   *slog.Logger

	// volatility maps a volatility label to duration used when storing
	// confirmed/rejected facts. Falls back to defaults when empty.
	volatility map[string]time.Duration

	// now is injectable for deterministic tests.
	now func() time.Time
}

// VerifierOptions bundles verifier construction parameters. Querier +
// AgentID + AgentShort are used to build memstore + learnstore clients
// internally.
type VerifierOptions struct {
	Querier    types.Querier
	AgentID    string
	AgentShort string
	Router     *models.Router
	Logger     *slog.Logger
	Volatility map[string]time.Duration
}

// NewVerifier builds a Verifier.
func NewVerifier(opts VerifierOptions) (*Verifier, error) {
	if opts.Querier == nil {
		return nil, fmt.Errorf("learning: Verifier requires Querier")
	}
	if opts.Router == nil {
		return nil, fmt.Errorf("learning: Verifier requires Router")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	memC, err := memstore.New(opts.Querier, memstore.Options{
		AgentID: opts.AgentID, AgentShort: opts.AgentShort, Logger: opts.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("learning: build memory store: %w", err)
	}
	learnC, err := learnstore.New(opts.Querier, learnstore.Options{
		AgentID: opts.AgentID, AgentShort: opts.AgentShort, Logger: opts.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("learning: build learning store: %w", err)
	}
	return &Verifier{
		memory:     memC,
		learning:   learnC,
		router:     opts.Router,
		logger:     opts.Logger,
		volatility: opts.Volatility,
		now:        func() time.Time { return time.Now().UTC() },
	}, nil
}

// VerifyNext picks one pending hypothesis (highest priority first,
// falling back to medium → low) and runs it. Returns nil when there
// is nothing to verify.
func (v *Verifier) VerifyNext(ctx context.Context) error {
	for _, priority := range []string{"high", "medium", "low"} {
		pending, err := v.learning.ListPendingHypotheses(ctx, priority, 1)
		if err != nil {
			return fmt.Errorf("learning: ListPendingHypotheses: %w", err)
		}
		if len(pending) == 0 {
			continue
		}
		return v.verify(ctx, pending[0])
	}
	return nil
}

// verify runs one hypothesis end-to-end: mark checking → run the LLM
// → parse verdict → Confirm / Reject / Defer. Failures mid-flight
// leave the row in `checking` — a subsequent Consolidator run can
// reset stuck rows, or the operator can inspect the hypothesis.
func (v *Verifier) verify(ctx context.Context, h learnstore.Hypothesis) error {
	if err := v.learning.MarkHypothesisChecking(ctx, h.ID); err != nil {
		return fmt.Errorf("learning: MarkChecking %q: %w", h.ID, err)
	}

	prompt := v.buildPrompt(h)
	llm := v.router.ModelFor(models.IntentToolCalling)
	raw, _, err := RunOnce(ctx, llm, prompt)
	if err != nil {
		_ = v.learning.DeferHypothesis(ctx, h.ID)
		return fmt.Errorf("learning: LLM: %w", err)
	}

	verdict, err := parseVerdict(raw)
	if err != nil {
		v.logger.Warn("verifier: unparseable verdict; deferring",
			"hypothesis", h.ID, "err", err)
		return v.learning.DeferHypothesis(ctx, h.ID)
	}

	switch verdict.Verdict {
	case "confirmed":
		return v.confirmHypothesis(ctx, h, verdict)
	case "rejected":
		return v.rejectHypothesis(ctx, h, verdict)
	default:
		v.logger.Info("verifier: inconclusive; deferring",
			"hypothesis", h.ID, "verdict", verdict.Verdict)
		return v.learning.DeferHypothesis(ctx, h.ID)
	}
}

func (v *Verifier) confirmHypothesis(ctx context.Context, h learnstore.Hypothesis, verdict verifierVerdict) error {
	now := v.now()
	dur := v.durationFor(defaultStr(verdict.Volatility, "stable"))
	factID, err := v.memory.Store(ctx, memstore.Item{
		Content:    h.Content + "\nEvidence: " + verdict.Evidence,
		Category:   defaultStr(h.Category, "data_insight"),
		Volatility: defaultStr(verdict.Volatility, "stable"),
		Score:      0.9, // verified → high confidence
		Source:     "hypothesis_verified:" + h.ID,
		ValidFrom:  now,
		ValidTo:    now.Add(dur),
	}, nil, nil)
	if err != nil {
		return fmt.Errorf("learning: Store confirmed fact: %w", err)
	}
	return v.learning.ConfirmHypothesis(ctx, h.ID, verdict.Evidence, factID)
}

func (v *Verifier) rejectHypothesis(ctx context.Context, h learnstore.Hypothesis, verdict verifierVerdict) error {
	now := v.now()
	dur := v.durationFor("moderate")
	_, err := v.memory.Store(ctx, memstore.Item{
		Content:    "Rejected hypothesis: " + h.Content + "\nReality: " + verdict.Evidence,
		Category:   "anti_pattern",
		Volatility: "moderate",
		Score:      0.7,
		Source:     "hypothesis_rejected:" + h.ID,
		ValidFrom:  now,
		ValidTo:    now.Add(dur),
	}, nil, nil)
	if err != nil {
		return fmt.Errorf("learning: Store anti-pattern: %w", err)
	}
	return v.learning.RejectHypothesis(ctx, h.ID, verdict.Evidence)
}

func (v *Verifier) buildPrompt(h learnstore.Hypothesis) string {
	var b strings.Builder
	b.WriteString("You are verifying a hypothesis derived from a prior agent session. Respond with a single JSON object describing your verdict — do not use tools.\n\n")
	b.WriteString("Hypothesis: ")
	b.WriteString(h.Content)
	b.WriteString("\n\n")
	if h.Verification != "" {
		b.WriteString("Suggested verification: ")
		b.WriteString(h.Verification)
		b.WriteString("\n\n")
	}
	b.WriteString(`Output format:
{"verdict": "confirmed|rejected|inconclusive", "evidence": "...", "volatility": "stable|slow|moderate|fast|volatile"}`)
	return b.String()
}

func (v *Verifier) durationFor(volatility string) time.Duration {
	if v.volatility != nil {
		if d, ok := v.volatility[volatility]; ok && d > 0 {
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
	return 365 * 24 * time.Hour
}

// verifierVerdict is the LLM's JSON response shape.
type verifierVerdict struct {
	Verdict    string `json:"verdict"`
	Evidence   string `json:"evidence"`
	Volatility string `json:"volatility"`
}

func parseVerdict(raw string) (verifierVerdict, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		if idx := strings.Index(raw, "\n"); idx >= 0 {
			raw = raw[idx+1:]
		}
		raw = strings.TrimSuffix(strings.TrimSpace(raw), "```")
	}
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end < 0 || end <= start {
		return verifierVerdict{}, fmt.Errorf("no JSON object in verdict")
	}
	var v verifierVerdict
	if err := json.Unmarshal([]byte(raw[start:end+1]), &v); err != nil {
		return verifierVerdict{}, err
	}
	return v, nil
}
