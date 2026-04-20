package system

import (
	"fmt"

	"github.com/hugr-lab/hugen/interfaces"
	"github.com/hugr-lab/hugen/pkg/tools"
	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"
)

// NewContextSuite returns the context-management tools exposed
// through the `_context` system provider: context_status,
// context_intro, context_compress. The compactor dependency is
// optional at this scaffolding stage — context_compress is wired
// up once pkg/learning.Compactor exists (spec 005 Phase 5).
//
// Each tool resolves its session from tool.Context and delegates
// to SessionManager.Session(id). No state is held by the suite.
func NewContextSuite(sm interfaces.SessionManager) []tool.Tool {
	return []tool.Tool{
		&contextStatusTool{sm: sm},
		// context_intro + context_compress land in Phase 5 of spec 005.
	}
}

// ------------------------------------------------------------
// context_status
// ------------------------------------------------------------

type contextStatusTool struct{ sm interfaces.SessionManager }

func (t *contextStatusTool) Name() string { return "context_status" }
func (t *contextStatusTool) Description() string {
	return "Returns current token usage: system prompt size and loaded tool count."
}
func (t *contextStatusTool) IsLongRunning() bool { return false }

func (t *contextStatusTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: t.Name(), Description: t.Description()}
}

func (t *contextStatusTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *contextStatusTool) Run(ctx tool.Context, _ any) (map[string]any, error) {
	sess, err := sessionFor(ctx, t.sm)
	if err != nil {
		return nil, fmt.Errorf("context_status: %w", err)
	}
	snap := sess.Snapshot()
	return map[string]any{
		"system_prompt_chars": len(snap.Prompt),
		"loaded_tools":        len(snap.Tools),
	}, nil
}
