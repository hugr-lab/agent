// Package agent builds the HugrAgent on top of ADK's llmagent, wiring
// our SessionManager for session lifecycle + tool injection and keeping
// ADK's Flow-level tool cache out of the picture (Toolsets: nil, all
// tool resolution happens in a BeforeModelCallback).
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/hugr-lab/hugen/interfaces"
	"github.com/hugr-lab/hugen/pkg/learning"
	"github.com/hugr-lab/hugen/pkg/llms/intent"
	"github.com/hugr-lab/hugen/pkg/tools"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
)

// Config bundles agent dependencies.
type Config struct {
	// Router is the intent-based LLM router.
	Router *intent.Router

	// Sessions provides per-invocation Snapshot (prompt + tools) and
	// session lifecycle. Required.
	Sessions interfaces.SessionManager

	// Tokens is the calibrateable estimator used by the AfterModelCallback
	// to track context usage per session. Optional.
	Tokens *learning.TokenEstimator

	// Logger is optional; defaults to slog.Default.
	Logger *slog.Logger
}

// NewAgent builds the HugrAgent:
//   - InstructionProvider → Session.Snapshot().Prompt
//   - BeforeModelCallbacks → tools.Inject(Sessions) (rewrites req tools)
//   - AfterModelCallbacks → calibrateTokens(Tokens) (updates token stats)
//   - Toolsets: nil — the Inject callback is the single source of truth
//     for both req.Config.Tools and req.Tools.
func NewAgent(cfg Config) (agent.Agent, error) {
	if cfg.Sessions == nil {
		return nil, fmt.Errorf("agent: Sessions required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	return llmagent.New(llmagent.Config{
		Name:        "hugr_agent",
		Description: "Hugr Data Mesh Agent — explores data sources, builds queries, presents results",
		Model:       cfg.Router,
		Toolsets:    nil,

		InstructionProvider: func(ctx agent.ReadonlyContext) (string, error) {
			sid := ctx.SessionID()
			if sid == "" {
				return "", nil
			}
			sess, err := cfg.Sessions.Session(sid)
			if err != nil {
				return "", fmt.Errorf("agent: instruction provider: %w", err)
			}
			return sess.Snapshot().Prompt, nil
		},

		BeforeModelCallbacks: []llmagent.BeforeModelCallback{
			tools.Inject(cfg.Sessions),
		},

		AfterModelCallbacks: []llmagent.AfterModelCallback{
			calibrateTokens(cfg),
		},
	})
}

// calibrateTokens returns an AfterModelCallback that feeds LLM usage
// metadata into the TokenEstimator.
func calibrateTokens(cfg Config) llmagent.AfterModelCallback {
	return func(ctx agent.CallbackContext, resp *model.LLMResponse, _ error) (*model.LLMResponse, error) {
		if resp == nil || resp.UsageMetadata == nil || cfg.Tokens == nil {
			return nil, nil
		}
		sid := ctx.SessionID()
		if sid == "" {
			return nil, nil
		}
		sess, err := cfg.Sessions.Session(sid)
		if err != nil {
			return nil, nil
		}
		promptTokens := int(resp.UsageMetadata.PromptTokenCount)
		completionTokens := int(resp.UsageMetadata.CandidatesTokenCount)
		promptChars := len(sess.Snapshot().Prompt)

		if promptTokens > 0 {
			cfg.Tokens.Calibrate(promptChars, promptTokens, completionTokens)
			if cfg.Logger != nil {
				cfg.Logger.Debug("token estimator calibrated",
					"prompt_tokens", promptTokens,
					"completion_tokens", completionTokens,
					"prompt_chars", promptChars,
					"source", cfg.Tokens.Source(),
				)
			}
		}
		return nil, nil
	}
}

// StartSessionCleanup launches a goroutine that periodically purges
// sessions inactive for more than maxAge. Cancel ctx to stop it.
func StartSessionCleanup(ctx context.Context, sm interfaces.SessionManager, maxAge time.Duration, logger *slog.Logger) {
	if sm == nil || maxAge <= 0 {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	go func() {
		ticker := time.NewTicker(maxAge / 2)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if n := sm.Cleanup(maxAge); n > 0 {
					logger.Info("session cleanup", "removed", n)
				}
			}
		}
	}()
}
