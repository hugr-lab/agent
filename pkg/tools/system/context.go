package system

import (
	hugen "github.com/hugr-lab/hugen/pkg/agent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"
)

// contextStatusTool reports current context token usage for the active session.
type contextStatusTool struct {
	prompt *hugen.PromptBuilder
	tokens *hugen.TokenEstimator
}

func (t *contextStatusTool) Name() string { return "context_status" }
func (t *contextStatusTool) Description() string {
	return "Returns current token usage: system prompt size, last LLM call tokens, and estimation source. Use before loading large references to check budget."
}
func (t *contextStatusTool) IsLongRunning() bool { return false }

func (t *contextStatusTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
	}
}

func (t *contextStatusTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	return packTool(req, t.Name(), t.Declaration(), t)
}

func (t *contextStatusTool) Run(ctx tool.Context, _ any) (map[string]any, error) {
	promptText := t.prompt.BuildForSession(ctx.SessionID())
	estimatedPromptTokens := t.tokens.Estimate(promptText)
	lastPrompt, lastCompletion := t.tokens.LastUsage()

	return map[string]any{
		"system_prompt_chars":    len(promptText),
		"system_prompt_tokens":   estimatedPromptTokens,
		"last_prompt_tokens":     lastPrompt,
		"last_completion_tokens": lastCompletion,
		"source":                 t.tokens.Source(),
		"note":                   "last_prompt/completion_tokens are global (across all sessions)",
	}, nil
}
