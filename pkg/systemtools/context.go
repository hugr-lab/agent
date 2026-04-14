package systemtools

import (
	"github.com/hugr-lab/agent/pkg/hugragent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"
)

// contextStatusTool reports current context token usage.
type contextStatusTool struct {
	prompt *hugragent.PromptBuilder
	tokens *hugragent.TokenEstimator
}

func (t *contextStatusTool) Name() string        { return "context_status" }
func (t *contextStatusTool) Description() string { return "Report current context token usage breakdown and budget" }
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

func (t *contextStatusTool) Run(_ tool.Context, _ any) (map[string]any, error) {
	promptText := t.prompt.Build()
	estimatedPromptTokens := t.tokens.Estimate(promptText)
	lastPrompt, lastCompletion := t.tokens.LastUsage()

	return map[string]any{
		"system_prompt_chars":    len(promptText),
		"system_prompt_tokens":   estimatedPromptTokens,
		"last_prompt_tokens":     lastPrompt,
		"last_completion_tokens": lastCompletion,
		"source":                 t.tokens.Source(),
	}, nil
}
