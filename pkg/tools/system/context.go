package system

import (
	"encoding/json"
	"fmt"

	"github.com/hugr-lab/hugen/interfaces"
	"github.com/hugr-lab/hugen/pkg/store"
	"github.com/hugr-lab/hugen/pkg/tools"
	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"
)

// OnDemandCompactor is what context_compress calls to trigger a
// manual compaction pass. Keeps pkg/tools/system free of a direct
// pkg/learning import — the runtime wires whichever concrete
// compactor it uses.
type OnDemandCompactor interface {
	Compact(ctx tool.Context) error
}

// NewContextSuite returns the context-management tools exposed
// through the `_context` system provider: context_status,
// context_intro, context_compress. compactor may be nil — in that
// case context_compress returns an informative error when invoked.
//
// Each tool resolves its session from tool.Context and delegates
// to SessionManager.Session(id) / HubDB. No state is held by the
// suite.
func NewContextSuite(sm interfaces.SessionManager, hub store.DB, compactor OnDemandCompactor) []tool.Tool {
	return []tool.Tool{
		&contextStatusTool{sm: sm},
		&contextIntroTool{sm: sm, hub: hub},
		&contextCompressTool{sm: sm, compactor: compactor},
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

// ------------------------------------------------------------
// context_intro
// ------------------------------------------------------------

type contextIntroTool struct {
	sm  interfaces.SessionManager
	hub store.DB
}

func (t *contextIntroTool) Name() string { return "context_intro" }
func (t *contextIntroTool) Description() string {
	return "Returns a short summary of what's currently in your context: loaded skills, loaded references, session note count, memory counts. Use it when you suspect old context is no longer relevant."
}
func (t *contextIntroTool) IsLongRunning() bool { return false }

func (t *contextIntroTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: t.Name(), Description: t.Description()}
}

func (t *contextIntroTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *contextIntroTool) Run(ctx tool.Context, _ any) (map[string]any, error) {
	sess, err := sessionFor(ctx, t.sm)
	if err != nil {
		return nil, fmt.Errorf("context_intro: %w", err)
	}
	snap := sess.Snapshot()
	out := map[string]any{
		"system_prompt_chars": len(snap.Prompt),
		"loaded_tools":        len(snap.Tools),
	}
	if t.hub != nil {
		if notes, err := t.hub.ListNotes(ctx, ctx.SessionID()); err == nil {
			out["session_notes"] = len(notes)
		}
		if stats, err := t.hub.Stats(ctx); err == nil {
			data, _ := json.Marshal(stats)
			out["memory"] = json.RawMessage(data)
		}
	}
	return out, nil
}

// ------------------------------------------------------------
// context_compress
// ------------------------------------------------------------

type contextCompressTool struct {
	sm        interfaces.SessionManager
	compactor OnDemandCompactor
}

func (t *contextCompressTool) Name() string { return "context_compress" }
func (t *contextCompressTool) Description() string {
	return "Asks the system to compress the oldest turn groups now instead of waiting for the automatic threshold. Does not change your current task — the compacted turns remain accessible via post-session review."
}
func (t *contextCompressTool) IsLongRunning() bool { return false }

func (t *contextCompressTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: t.Name(), Description: t.Description()}
}

func (t *contextCompressTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *contextCompressTool) Run(ctx tool.Context, _ any) (map[string]any, error) {
	if t.compactor == nil {
		return map[string]any{"compressed": false, "reason": "compactor not wired"}, nil
	}
	if err := t.compactor.Compact(ctx); err != nil {
		return nil, fmt.Errorf("context_compress: %w", err)
	}
	return map[string]any{"compressed": true}, nil
}
