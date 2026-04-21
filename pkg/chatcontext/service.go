package chatcontext

import (
	"encoding/json"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/sessions"
	"github.com/hugr-lab/hugen/pkg/store"
	"github.com/hugr-lab/hugen/pkg/tools"
	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"
)

// ServiceName is the provider name this service registers under in
// tools.Manager. On-disk `_context/SKILL.md` references it via
// providers: [{provider: _context}].
const ServiceName = "_context"

// Service is the tools.Provider that exposes context-window tools
// (context_status, context_intro). Compaction is automatic via the
// Compactor BeforeModelCallback — no context_compress tool is
// exposed.
type Service struct {
	sm    *sessions.Manager
	hub   store.DB
	tools []tool.Tool
}

// NewService constructs the Service. hub may be nil — context_intro
// then returns just prompt/tool counts without hub-level stats.
func NewService(sm *sessions.Manager, hub store.DB) *Service {
	s := &Service{sm: sm, hub: hub}
	s.tools = []tool.Tool{
		&contextStatusTool{sm: sm},
		&contextIntroTool{sm: sm, hub: hub},
	}
	return s
}

// Name implements tools.Provider.
func (s *Service) Name() string { return ServiceName }

// Tools implements tools.Provider.
func (s *Service) Tools() []tool.Tool { return s.tools }

// sessionFor resolves the current session from the tool context.
func sessionFor(ctx tool.Context, sm *sessions.Manager) (*sessions.Session, error) {
	sid := ctx.SessionID()
	if sid == "" {
		return nil, fmt.Errorf("no session id in tool context")
	}
	return sm.Session(sid)
}

// ------------------------------------------------------------
// context_status
// ------------------------------------------------------------

type contextStatusTool struct{ sm *sessions.Manager }

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
	sm  *sessions.Manager
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

