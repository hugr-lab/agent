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

	"github.com/hugr-lab/hugen/pkg/models"
	"github.com/hugr-lab/hugen/pkg/sessions"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
)

// Config bundles agent dependencies.
type Config struct {
	// Router is the intent-based LLM router.
	Router *models.Router

	// Sessions provides per-invocation Snapshot (prompt + tools) and
	// session lifecycle. Required.
	Sessions *sessions.Manager

	// Tokens is the calibrateable estimator used by the AfterModelCallback
	// to track context usage per session. Optional.
	Tokens *models.TokenEstimator

	// ExtraBeforeCallbacks are appended to the BeforeModelCallbacks
	// chain after tools.Inject. Order matters: the runtime ships with
	// [tools.Inject, chatcontext.Compactor.Before], ensuring the compactor
	// operates on a tools-aware request and runs last before the model.
	ExtraBeforeCallbacks []llmagent.BeforeModelCallback

	// InstructionProvider overrides the default `Session.Snapshot().Prompt`
	// provider. Runtime uses memory.WrapInstruction to append a
	// "## Memory Status" block on top of the session's base prompt.
	// When nil, the default (state-only Snapshot prompt) is used.
	InstructionProvider llmagent.InstructionProvider

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

	baseInstruction := func(ctx agent.ReadonlyContext) (string, error) {
		sid := ctx.SessionID()
		if sid == "" {
			return "", nil
		}
		sess, err := cfg.Sessions.Session(sid)
		if err != nil {
			return "", fmt.Errorf("agent: instruction provider: %w", err)
		}
		return sess.Snapshot().Prompt, nil
	}
	instruction := cfg.InstructionProvider
	if instruction == nil {
		instruction = baseInstruction
	}

	return llmagent.New(llmagent.Config{
		Name:        "hugr_agent",
		Description: "Hugr Data Mesh Agent — explores data sources, builds queries, presents results",
		Model:       cfg.Router,
		Toolsets:    nil,

		InstructionProvider: instruction,

		BeforeModelCallbacks: append(
			[]llmagent.BeforeModelCallback{sessions.Inject(cfg.Sessions)},
			cfg.ExtraBeforeCallbacks...,
		),

		AfterModelCallbacks: []llmagent.AfterModelCallback{
			calibrateTokens(cfg),
		},
	})
}

// BaseInstructionProvider returns the default instruction provider:
// reads the current session's Snapshot().Prompt. Exposed so the
// runtime can wrap it (e.g. memory.WrapInstruction for the memory
// status hint) before handing the composed provider back via
// Config.InstructionProvider.
func BaseInstructionProvider(sm *sessions.Manager) llmagent.InstructionProvider {
	return func(ctx agent.ReadonlyContext) (string, error) {
		sid := ctx.SessionID()
		if sid == "" {
			return "", nil
		}
		sess, err := sm.Session(sid)
		if err != nil {
			return "", fmt.Errorf("agent: instruction provider: %w", err)
		}
		return sess.Snapshot().Prompt, nil
	}
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
func StartSessionCleanup(ctx context.Context, sm *sessions.Manager, maxAge time.Duration, logger *slog.Logger) {
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
