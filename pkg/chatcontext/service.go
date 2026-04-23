package chatcontext

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/hugr-lab/hugen/pkg/sessions"
	memstore "github.com/hugr-lab/hugen/pkg/memory/store"
	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
	"github.com/hugr-lab/hugen/pkg/tools"
	"github.com/hugr-lab/query-engine/types"
	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"
)

// ServiceName is the provider name this service registers under in
// tools.Manager. On-disk `_context/SKILL.md` references it via
// providers: [{provider: _context}].
const ServiceName = "_context"

// ServiceOptions bundles service construction parameters. AgentID /
// AgentShort are forwarded to the internal store clients. Logger may
// be nil.
type ServiceOptions struct {
	AgentID    string
	AgentShort string
	Logger     *slog.Logger

	// Memory / Sessions are optional pre-built clients. When set, the
	// service skips the per-subsystem New() call. Preferred wiring
	// from runtime.go so every consumer shares the same instance.
	Memory   *memstore.Client
	Sessions *sessstore.Client
}

// Service is the tools.Provider that exposes context-window tools
// (context_status, context_intro). Compaction is automatic via the
// Compactor BeforeModelCallback — no context_compress tool is
// exposed.
type Service struct {
	sm       *sessions.Manager
	memory   *memstore.Client
	sessions *sessstore.Client
	tools    []tool.Tool
}

// NewService constructs the Service. querier may be nil — context_intro
// then returns just prompt/tool counts without hub-level stats. The
// service builds its own memstore + sessstore clients internally.
func NewService(querier types.Querier, sm *sessions.Manager, opts ServiceOptions) (*Service, error) {
	s := &Service{sm: sm, memory: opts.Memory, sessions: opts.Sessions}
	if querier != nil {
		if s.memory == nil {
			memC, err := memstore.New(querier, memstore.Options{
				AgentID: opts.AgentID, AgentShort: opts.AgentShort, Logger: opts.Logger,
			})
			if err != nil {
				return nil, fmt.Errorf("chatcontext: build memory store: %w", err)
			}
			s.memory = memC
		}
		if s.sessions == nil {
			sessC, err := sessstore.New(querier, sessstore.Options{
				AgentID: opts.AgentID, AgentShort: opts.AgentShort, Logger: opts.Logger,
			})
			if err != nil {
				return nil, fmt.Errorf("chatcontext: build sessions store: %w", err)
			}
			s.sessions = sessC
		}
	}
	s.tools = []tool.Tool{
		&contextStatusTool{sm: sm},
		&contextIntroTool{sm: sm, memory: s.memory, sessions: s.sessions},
	}
	return s, nil
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
	sm       *sessions.Manager
	memory   *memstore.Client
	sessions *sessstore.Client
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
	if t.sessions != nil {
		if notes, err := t.sessions.ListNotes(ctx, ctx.SessionID()); err == nil {
			out["session_notes"] = len(notes)
		}
	}
	if t.memory != nil {
		if stats, err := t.memory.Stats(ctx); err == nil {
			data, _ := json.Marshal(stats)
			out["memory"] = json.RawMessage(data)
		}
	}
	return out, nil
}
