package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/hugr-lab/hugen/pkg/llms/intent"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
)

// AgentConfig holds the configuration for building a HugrAgent.
type AgentConfig struct {
	// Router is the intent-based LLM router.
	Router *intent.Router

	// Toolset is the dynamic toolset that manages all runtime tools.
	Toolset *DynamicToolset

	// Prompt builds the system prompt from constitution + skills.
	Prompt *PromptBuilder

	// Tokens estimates and calibrates context token usage.
	Tokens *TokenEstimator

	// Logger for agent operations.
	Logger *slog.Logger

	// Debug enables verbose channel events with full tool args/results.
	Debug bool
}

// NewAgent creates a Hugr agent backed by llmagent with dynamic prompt, tools,
// and intent-based LLM routing.
//
// The agent uses llmagent.New() with:
//   - InstructionProvider → PromptBuilder.BuildForSession() (per-session system prompt)
//   - Toolsets → [DynamicToolset] (tools change when skills load/unload)
//   - Model → IntentLLM Router (routes by intent, Phase 2: single model)
//   - AfterModelCallbacks → TokenEstimator calibration
//
// Skill state (instructions, catalog, references) is session-scoped inside
// PromptBuilder, so parallel sessions never interfere with each other.
func NewAgent(cfg AgentConfig) (agent.Agent, error) {
	return llmagent.New(llmagent.Config{
		Name:        "hugr_agent",
		Description: "Hugr Data Mesh Agent — explores data sources, builds queries, presents results",
		Model:       cfg.Router,
		Toolsets:    []tool.Toolset{cfg.Toolset},

		InstructionProvider: func(ctx agent.ReadonlyContext) (string, error) {
			return cfg.Prompt.BuildForSession(ctx.SessionID()), nil
		},

		AfterModelCallbacks: []llmagent.AfterModelCallback{
			calibrateTokens(cfg),
		},
	})
}

// calibrateTokens returns a callback that feeds LLM usage metadata
// into the TokenEstimator after each model response.
func calibrateTokens(cfg AgentConfig) llmagent.AfterModelCallback {
	return func(ctx agent.CallbackContext, resp *model.LLMResponse, _ error) (*model.LLMResponse, error) {
		if resp == nil || resp.UsageMetadata == nil {
			return nil, nil
		}

		promptTokens := int(resp.UsageMetadata.PromptTokenCount)
		completionTokens := int(resp.UsageMetadata.CandidatesTokenCount)
		promptChars := cfg.Prompt.CharCountForSession(ctx.SessionID())

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

		// Return nil to not alter the response.
		return nil, nil
	}
}

// StartSessionCleanup starts a background goroutine that periodically removes
// stale session state from PromptBuilder and DynamicToolset.
// Call cancel on the returned context to stop the goroutine.
func StartSessionCleanup(ctx context.Context, prompt *PromptBuilder, toolset *DynamicToolset, maxAge time.Duration, logger *slog.Logger) {
	go func() {
		ticker := time.NewTicker(maxAge / 2)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p := prompt.CleanupStaleSessions(maxAge)
				t := toolset.CleanupStaleSessions(maxAge)
				if p+t > 0 && logger != nil {
					logger.Info("session cleanup", "prompt_sessions", p, "toolset_sessions", t)
				}
			}
		}
	}()
}
