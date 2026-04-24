// Package followup is the coordinator's mid-flight refinement router.
// A user message arriving while one or more missions are running can
// be a *refinement* of a specific running mission ("oh, only high
// severity") rather than a fresh request. This package classifies the
// message against the set of running missions and — when exactly one
// match is unambiguously above threshold — reroutes the message to
// that mission's session instead of spawning a duplicate plan.
//
// Exposed as a BeforeModelCallback on the coordinator agent's chain.
// On the route branch it returns a synthesised LLMResponse that
// short-circuits the model call; every other branch returns nil so
// normal coordinator planning proceeds.
package followup

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/hugr-lab/hugen/pkg/missions/executor"
	"github.com/hugr-lab/hugen/pkg/missions/graph"
	"github.com/hugr-lab/hugen/pkg/models"
)

// Defaults for the classifier-confidence gates. Threshold is the
// floor; TieBand is the slack zone above the floor where a single
// match still isn't trusted (treated as ambiguous — proceed without
// routing). Together: route iff confidence >= Threshold+TieBand.
const (
	defaultThreshold = 0.55
	defaultTieBand   = 0.05
	defaultTimeout   = 3 * time.Second
)

// defaultPromptHeader is the embedded fallback used when the runtime
// hasn't supplied a PromptHeader (typically: unit tests with no
// filesystem). Production loads the prose from
// skills/_coordinator/followup-classifier.md so operators can edit
// the classification rules without rebuilding.
const defaultPromptHeader = `Classify whether the user message is a refinement of a specific running mission or a new request. A refinement narrows or redirects what the mission is currently doing (e.g. "focus only on high-severity", "use the 2024 data"). Meta-action requests about a mission — cancel, stop, abandon, pause, resume, or inspect — are NOT refinements and must return match=null so the coordinator can act on them itself.

Reply ONLY with JSON of the shape {"match": <integer id from the list or null>, "confidence": <0.0-1.0>}. Set ` + "`match`" + ` to null when unsure, when the message is clearly a new topic, or when it is a meta-action (cancel / stop / status / etc).`

// Router decides, per coordinator turn, whether to reroute the
// incoming user message into a running mission.
type Router struct {
	executor     *executor.Executor
	router       *models.Router
	threshold    float64
	tieBand      float64
	enabled      bool
	timeout      time.Duration
	logger       *slog.Logger
	promptHeader string
}

// Config bundles the Router's construction dependencies.
type Config struct {
	Executor *executor.Executor
	Router   *models.Router
	Logger   *slog.Logger

	// Threshold / TieBand guard classifier confidence. Zero values
	// fall back to the defaults (0.55 / 0.05).
	Threshold float64
	TieBand   float64

	// Timeout caps the classifier call. Zero falls back to 3s.
	Timeout time.Duration

	// Enabled flips routing on/off at runtime. Zero-value (false)
	// means disabled — callers opt in from config.
	Enabled bool

	// PromptHeader is the operator-editable instruction body the
	// classifier sees before the running-missions list and the user
	// message. Empty falls back to defaultPromptHeader so unit tests
	// stay filesystem-free; runtime loads it from
	// skills/_coordinator/followup-classifier.md.
	PromptHeader string
}

// New builds a Router. Nil Executor or nil Router → routing is
// effectively disabled even when Enabled=true (the callback safely
// proceeds).
func New(cfg Config) *Router {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Threshold <= 0 {
		cfg.Threshold = defaultThreshold
	}
	if cfg.TieBand <= 0 {
		cfg.TieBand = defaultTieBand
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	header := strings.TrimSpace(cfg.PromptHeader)
	if header == "" {
		header = defaultPromptHeader
	}
	cfg.Logger.Info("missions/followup: router built",
		"enabled", cfg.Enabled,
		"threshold", cfg.Threshold,
		"tie_band", cfg.TieBand,
		"timeout", cfg.Timeout,
		"prompt_header_loaded", cfg.PromptHeader != "")
	return &Router{
		executor:     cfg.Executor,
		router:       cfg.Router,
		threshold:    cfg.Threshold,
		tieBand:      cfg.TieBand,
		enabled:      cfg.Enabled,
		timeout:      cfg.Timeout,
		logger:       cfg.Logger,
		promptHeader: header,
	}
}

// Callback is the BeforeModelCallback. Position in the coordinator's
// chain: AFTER the tools-inject callback (so the session id is on
// ctx) and BEFORE the compactor (so the short-circuit skips a pointless
// compaction). See contracts/followup-router.md §6.
func (r *Router) Callback() llmagent.BeforeModelCallback {
	return func(ctx agent.CallbackContext, req *model.LLMRequest) (*model.LLMResponse, error) {
		// agent.CallbackContext embeds context.Context via
		// ReadonlyContext — pass it straight through.
		return r.Decide(ctx, ctx.SessionID(), req)
	}
}

// Decide is the tool-context-free entry point for the router's
// business logic. Tests call it directly with a plain context.Context
// + session id + LLMRequest. Callback() is a thin adapter around it
// for the ADK BeforeModelCallback shape.
//
// Returns a non-nil LLMResponse to short-circuit the model call
// (routed path), or nil to let the normal coordinator turn proceed.
func (r *Router) Decide(
	ctx context.Context,
	coordID string,
	req *model.LLMRequest,
) (*model.LLMResponse, error) {
	if !r.enabled || r.executor == nil || r.router == nil {
		return nil, nil
	}
	if coordID == "" {
		return nil, nil
	}
	running := r.executor.RunningMissions(coordID)
	if len(running) == 0 {
		return nil, nil
	}
	userMsg := latestUserMessage(req)
	if userMsg == "" {
		return nil, nil
	}

	match, confidence, err := r.classify(ctx, userMsg, running)
	if err != nil {
		r.logger.WarnContext(ctx, "missions/followup: classifier error — proceed",
			"coord", coordID, "err", err)
		return nil, nil
	}
	r.logger.InfoContext(ctx, "missions/followup: classifier decision",
		"coord", coordID,
		"user_msg", truncateForAck(userMsg, 80),
		"match", match,
		"confidence", confidence,
		"threshold", r.threshold+r.tieBand,
		"running_missions", len(running))
	if match == "" || confidence < r.threshold+r.tieBand {
		return nil, nil
	}

	if err := r.executor.OnFollowUp(ctx, coordID, userMsg, match, confidence); err != nil {
		r.logger.WarnContext(ctx, "missions/followup: OnFollowUp failed — proceed",
			"coord", coordID, "target", match, "err", err)
		return nil, nil
	}

	target := missionByID(running, match)
	reply := fmt.Sprintf("Relaying to mission %s: '%s'.",
		roleLabel(target), truncateForAck(userMsg, 80))
	return &model.LLMResponse{
		Content: &genai.Content{
			Role:  "model",
			Parts: []*genai.Part{{Text: reply}},
		},
		TurnComplete: true,
	}, nil
}

// classifierOutput is the strict JSON shape the classifier LLM is
// told to return. Non-matching output → classify returns ("", 0, err).
type classifierOutput struct {
	Match      any     `json:"match"`
	Confidence float64 `json:"confidence"`
}

// classify asks the LLM to pair the user message against each running
// mission. Returns the matched mission id + confidence, or ("", 0, err)
// on parse / transport failure. Timeout bounded by r.timeout.
func (r *Router) classify(
	ctx context.Context,
	userMsg string,
	running []graph.MissionRecord,
) (string, float64, error) {
	llm := r.router.ModelFor(models.IntentClassification)
	if llm == nil {
		return "", 0, fmt.Errorf("router returned nil classifier model")
	}

	prompt := buildPrompt(r.promptHeader, userMsg, running)
	req := &model.LLMRequest{
		Contents: []*genai.Content{{
			Role:  "user",
			Parts: []*genai.Part{{Text: prompt}},
		}},
	}

	callCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	var text strings.Builder
	for resp, err := range llm.GenerateContent(callCtx, req, false) {
		if err != nil {
			return "", 0, fmt.Errorf("classifier llm: %w", err)
		}
		if resp == nil || resp.Content == nil {
			continue
		}
		for _, p := range resp.Content.Parts {
			if p != nil && p.Text != "" {
				text.WriteString(p.Text)
			}
		}
		if resp.TurnComplete {
			break
		}
	}

	return parseOutput(text.String(), running)
}

// parseOutput pulls out match + confidence. Match may arrive as int
// (mission index 1..N referencing the enumerated list) or as string
// (the mission session id directly). Either way we resolve to a real
// running.ID before returning. Fences stripped defensively.
func parseOutput(raw string, running []graph.MissionRecord) (string, float64, error) {
	text := strings.TrimSpace(raw)
	if strings.HasPrefix(text, "```") {
		if idx := strings.Index(text, "\n"); idx >= 0 {
			text = text[idx+1:]
		}
		text = strings.TrimSuffix(text, "```")
		text = strings.TrimSpace(text)
	}
	dec := json.NewDecoder(strings.NewReader(text))
	var out classifierOutput
	if err := dec.Decode(&out); err != nil {
		return "", 0, fmt.Errorf("parse classifier output: %w", err)
	}

	switch v := out.Match.(type) {
	case nil:
		return "", out.Confidence, nil
	case string:
		for _, m := range running {
			if m.ID == v {
				return m.ID, out.Confidence, nil
			}
		}
		return "", 0, fmt.Errorf("classifier returned unknown mission id %q", v)
	case float64:
		idx := int(v) - 1 // 1-based in the prompt
		if idx < 0 || idx >= len(running) {
			return "", 0, fmt.Errorf("classifier returned out-of-range index %d", int(v))
		}
		return running[idx].ID, out.Confidence, nil
	default:
		return "", 0, fmt.Errorf("classifier match has unsupported type %T", out.Match)
	}
}

// buildPrompt renders the operator-supplied instruction header (loaded
// from skills/_coordinator/followup-classifier.md in production, the
// embedded default in tests) followed by the structured running-
// missions list and the verbatim user message. The header carries the
// classification rules — including the meta-action carve-out so
// cancel/stop/inspect requests aren't rerouted into child transcripts.
func buildPrompt(header, userMsg string, running []graph.MissionRecord) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(header))
	b.WriteString("\n\nRunning missions:\n")
	for i, m := range running {
		fmt.Fprintf(&b, "  [%d] %s/%s: %s\n", i+1, m.Skill, m.Role, truncateForPrompt(m.Task, 160))
	}
	b.WriteString("\nUser message: ")
	b.WriteString(userMsg)
	b.WriteString("\n")
	return b.String()
}

// latestUserMessage returns the verbatim text of the most recent
// user turn in the request. Empty string when there isn't one.
func latestUserMessage(req *model.LLMRequest) string {
	if req == nil {
		return ""
	}
	for i := len(req.Contents) - 1; i >= 0; i-- {
		c := req.Contents[i]
		if c == nil || c.Role != "user" {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && strings.TrimSpace(p.Text) != "" {
				return p.Text
			}
		}
	}
	return ""
}

func missionByID(running []graph.MissionRecord, id string) graph.MissionRecord {
	for _, m := range running {
		if m.ID == id {
			return m
		}
	}
	return graph.MissionRecord{}
}

func roleLabel(m graph.MissionRecord) string {
	if m.Role == "" {
		return m.ID
	}
	return m.Role
}

func truncateForAck(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func truncateForPrompt(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
