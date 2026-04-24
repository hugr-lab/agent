package memory

import (
	"context"
	"encoding/json"
	"errors"
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
// tools.Manager. On-disk `_memory/SKILL.md` references it via
// providers: [{provider: _memory}].
const ServiceName = "_memory"

// ServiceOptions bundles service construction parameters. AgentID and
// AgentShort are forwarded to the internal store clients. Logger may be
// nil.
//
// Memory / Sessions fields let the runtime inject pre-built store
// clients. When unset, NewService falls back to constructing them
// from the querier parameter.
type ServiceOptions struct {
	AgentID    string
	AgentShort string
	Logger     *slog.Logger

	Memory   *memstore.Client
	Sessions *sessstore.Client
}

// Service is the tools.Provider that exposes long-term memory tools
// (memory_search / memory_linked / memory_stats / memory_note /
// memory_clear_note). Stateless beyond the hub + session-manager
// references it holds.
type Service struct {
	sm       *sessions.Manager
	memory   *memstore.Client
	sessions *sessstore.Client
	tools    []tool.Tool
}

// NewService returns the memory tools provider. When querier is nil the
// service exposes no tools (Tools() → empty slice); registering it
// anyway keeps the provider catalogue consistent. The service builds
// its own memstore + sessstore clients internally from the given
// querier.
//
// Spec 006 §3 dropped the direct Go-side embedding client dependency:
// memory_search sends the raw query string to Hugr's `semantic:`
// argument, which embeds server-side. No caller of NewService needs
// to pass an embedding client any more.
func NewService(querier types.Querier, sm *sessions.Manager, opts ServiceOptions) (*Service, error) {
	s := &Service{sm: sm}
	memC := opts.Memory
	sessC := opts.Sessions
	if memC == nil && querier != nil {
		c, err := memstore.New(querier, memstore.Options{
			AgentID: opts.AgentID, AgentShort: opts.AgentShort, Logger: opts.Logger,
		})
		if err != nil {
			return nil, fmt.Errorf("memory: build memory store: %w", err)
		}
		memC = c
	}
	if sessC == nil && querier != nil {
		c, err := sessstore.New(querier, sessstore.Options{
			AgentID: opts.AgentID, AgentShort: opts.AgentShort, Logger: opts.Logger,
		})
		if err != nil {
			return nil, fmt.Errorf("memory: build sessions store: %w", err)
		}
		sessC = c
	}
	if memC == nil || sessC == nil {
		return s, nil
	}
	s.memory = memC
	s.sessions = sessC
	s.tools = []tool.Tool{
		&memorySearchTool{sm: sm, memory: memC},
		&memoryLinkedTool{sm: sm, memory: memC},
		&memoryStatsTool{sm: sm, memory: memC},
		&memoryNoteTool{sm: sm, sessions: sessC},
		&memoryClearNoteTool{sm: sm, sessions: sessC},
	}
	return s, nil
}

// Name implements tools.Provider.
func (s *Service) Name() string { return ServiceName }

// Tools implements tools.Provider.
func (s *Service) Tools() []tool.Tool { return s.tools }

// ------------------------------------------------------------
// memory_search
// ------------------------------------------------------------

type memorySearchTool struct {
	sm     *sessions.Manager
	memory *memstore.Client
}

func (t *memorySearchTool) Name() string { return "memory_search" }
func (t *memorySearchTool) Description() string {
	return "Searches long-term memory for facts matching a natural-language query. Filter by tags (AND) or category. Returns at most 5 facts by default, each with age_days and expires_in_days so you can judge freshness."
}
func (t *memorySearchTool) IsLongRunning() bool { return false }

func (t *memorySearchTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: "OBJECT",
			Properties: map[string]*genai.Schema{
				"query": {
					Type:        "STRING",
					Description: "Natural-language query. Matched against fact content with keyword fallback when no embedding is available.",
				},
				"tags": {
					Type:        "ARRAY",
					Items:       &genai.Schema{Type: "STRING"},
					Description: "Optional tag filter (AND). Facts must carry every listed tag.",
				},
				"category": {
					Type:        "STRING",
					Description: "Optional category filter. Valid category names are listed under each active skill's \"Memory categories\" block in the system prompt. Call memory_stats to see which categories currently have facts.",
				},
				"limit": {
					Type:        "INTEGER",
					Description: "Max results to return. Default 5, hard cap 20.",
				},
			},
			Required: []string{"query"},
		},
	}
}

func (t *memorySearchTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *memorySearchTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	m, ok := args.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("memory_search: unexpected args: %T", args)
	}
	query, _ := m["query"].(string)
	category, _ := m["category"].(string)
	var tags []string
	if raw, ok := m["tags"].([]any); ok {
		for _, v := range raw {
			if s, _ := v.(string); s != "" {
				tags = append(tags, s)
			}
		}
	}
	limit := 5
	if v, ok := m["limit"].(float64); ok && v > 0 {
		limit = int(v)
		if limit > 20 {
			limit = 20
		}
	}
	results, err := t.memory.Search(ctx, query, memstore.SearchOpts{
		Category: category,
		Tags:     tags,
		Limit:    limit,
	})
	if err != nil {
		return nil, fmt.Errorf("memory_search: %w", err)
	}
	data, _ := json.Marshal(results)
	return map[string]any{"results": json.RawMessage(data), "count": len(results)}, nil
}

// ------------------------------------------------------------
// memory_linked
// ------------------------------------------------------------

type memoryLinkedTool struct {
	sm     *sessions.Manager
	memory *memstore.Client
}

func (t *memoryLinkedTool) Name() string { return "memory_linked" }
func (t *memoryLinkedTool) Description() string {
	return "Returns facts reachable from a given memory item through outgoing links, up to the requested depth. Use to navigate from a schema fact to related query templates and anti-patterns."
}
func (t *memoryLinkedTool) IsLongRunning() bool { return false }

func (t *memoryLinkedTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: "OBJECT",
			Properties: map[string]*genai.Schema{
				"id":    {Type: "STRING", Description: "Memory item ID (e.g. mem_ag01_...)"},
				"depth": {Type: "INTEGER", Description: "Traversal depth. Default 1, max 3."},
			},
			Required: []string{"id"},
		},
	}
}

func (t *memoryLinkedTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *memoryLinkedTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	m, ok := args.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("memory_linked: unexpected args: %T", args)
	}
	id, _ := m["id"].(string)
	if id == "" {
		return nil, fmt.Errorf("memory_linked: missing required parameter: id")
	}
	depth := 1
	if v, ok := m["depth"].(float64); ok && v > 0 {
		depth = int(v)
		if depth > 3 {
			depth = 3
		}
	}
	results, err := t.memory.GetLinked(ctx, id, depth)
	if err != nil {
		return nil, fmt.Errorf("memory_linked: %w", err)
	}
	data, _ := json.Marshal(results)
	return map[string]any{"results": json.RawMessage(data), "count": len(results)}, nil
}

// ------------------------------------------------------------
// memory_stats
// ------------------------------------------------------------

type memoryStatsTool struct {
	sm     *sessions.Manager
	memory *memstore.Client
}

func (t *memoryStatsTool) Name() string { return "memory_stats" }
func (t *memoryStatsTool) Description() string {
	return "Returns a compact summary of long-term memory: total count, active count, counts by category, oldest/newest fact dates."
}
func (t *memoryStatsTool) IsLongRunning() bool { return false }

func (t *memoryStatsTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: t.Name(), Description: t.Description()}
}

func (t *memoryStatsTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *memoryStatsTool) Run(ctx tool.Context, _ any) (map[string]any, error) {
	stats, err := t.memory.Stats(ctx)
	if err != nil {
		return nil, fmt.Errorf("memory_stats: %w", err)
	}
	data, _ := json.Marshal(stats)
	return map[string]any{"stats": json.RawMessage(data)}, nil
}

// ------------------------------------------------------------
// memory_note (session scratchpad)
// ------------------------------------------------------------

type memoryNoteTool struct {
	sm       *sessions.Manager
	sessions *sessstore.Client
}

func (t *memoryNoteTool) Name() string { return "memory_note" }
func (t *memoryNoteTool) Description() string {
	return "Saves a short finding to the session scratchpad. The note is visible in your prompt for the rest of this session and survives context compaction. Optional scope promotes the note up the session chain: 'parent' writes to the coordinator that dispatched you, 'ancestors' writes to every session above. Returns the note id(s) so you can clear them later (author-only)."
}
func (t *memoryNoteTool) IsLongRunning() bool { return false }

func (t *memoryNoteTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: "OBJECT",
			Properties: map[string]*genai.Schema{
				"content": {
					Type:        "STRING",
					Description: "Concise, self-contained note text. Prefer ≤ 150 chars per note.",
				},
				"scope": {
					Type:        "STRING",
					Description: "Where the note is visible: 'self' (default, current session only), 'parent' (target the session that dispatched this one; error on root), 'ancestors' (walk the whole chain upward, one note per ancestor).",
					Enum:        []string{"self", "parent", "ancestors"},
				},
			},
			Required: []string{"content"},
		},
	}
}

func (t *memoryNoteTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *memoryNoteTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	m, ok := args.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("memory_note: unexpected args: %T", args)
	}
	content, _ := m["content"].(string)
	if content == "" {
		return nil, fmt.Errorf("memory_note: missing required parameter: content")
	}
	scope, _ := m["scope"].(string)
	if scope == "" {
		scope = "self"
	}
	switch scope {
	case "self", "parent", "ancestors":
	default:
		return nil, fmt.Errorf("memory_note: unknown scope %q (want self|parent|ancestors)", scope)
	}
	sid := ctx.SessionID()
	if sid == "" {
		return nil, fmt.Errorf("memory_note: no session id in tool context")
	}

	// Resolve the list of session ids that should receive the note.
	// For self the author and session target are the same row; for
	// parent / ancestors the author stays at the current session and
	// the target id(s) walk up the chain.
	current, err := t.sm.Session(sid)
	if err != nil {
		return nil, fmt.Errorf("memory_note: %w", err)
	}
	targets, err := resolveNoteTargets(ctx, t.sessions, current, scope)
	if err != nil {
		return nil, fmt.Errorf("memory_note: %w", err)
	}

	ids := make([]string, 0, len(targets))
	for _, targetID := range targets {
		id, err := t.sessions.AddNote(ctx, sessstore.Note{
			SessionID:       targetID,
			AuthorSessionID: sid,
			Content:         content,
		})
		if err != nil {
			return nil, fmt.Errorf("memory_note: %w", err)
		}
		ids = append(ids, id)
		// Invalidate every affected session's render cache. Own cache
		// gets invalidated implicitly when the current session later
		// walks the chain and finds its own notes; upstream ones need
		// an explicit poke since their Snapshot() may otherwise serve
		// a stale cached block for up to 10 s.
		if sess, err := t.sm.Session(targetID); err == nil {
			sess.InvalidateNotesCache()
		}
	}
	// Always invalidate the author's cache so the LLM sees their own
	// note on the very next Snapshot (the view returns own notes at
	// chain_depth = 0 regardless of scope).
	current.InvalidateNotesCache()

	out := map[string]any{"saved": true, "scope": scope}
	if len(ids) == 1 {
		out["id"] = ids[0]
	} else {
		out["ids"] = ids
	}
	return out, nil
}

// resolveNoteTargets maps a memory_note scope to the target session
// ids that receive a row. "self" returns just the caller; "parent"
// returns the caller's parent (or errors on root); "ancestors" walks
// parent_session_id upward through the hub, capped at depth 8 to
// mirror session_notes_chain's cap.
func resolveNoteTargets(ctx context.Context, hub *sessstore.Client, current *sessions.Session, scope string) ([]string, error) {
	switch scope {
	case "self":
		return []string{current.ID()}, nil
	case "parent":
		parent := current.ParentSessionID()
		if parent == "" {
			return nil, errors.New("no parent session (scope requires a sub-agent / fork)")
		}
		return []string{parent}, nil
	case "ancestors":
		parent := current.ParentSessionID()
		if parent == "" {
			return nil, errors.New("no ancestor sessions (scope requires a sub-agent / fork)")
		}
		var out []string
		cur := parent
		for i := 0; i < 8 && cur != ""; i++ {
			out = append(out, cur)
			rec, err := hub.GetSession(ctx, cur)
			if err != nil || rec == nil {
				break
			}
			cur = rec.ParentSessionID
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unknown scope %q", scope)
	}
}

// ------------------------------------------------------------
// memory_clear_note
// ------------------------------------------------------------

type memoryClearNoteTool struct {
	sm       *sessions.Manager
	sessions *sessstore.Client
}

func (t *memoryClearNoteTool) Name() string { return "memory_clear_note" }
func (t *memoryClearNoteTool) Description() string {
	return "Removes a previously saved session note by its id. Useful when the finding is no longer relevant for the remaining task."
}
func (t *memoryClearNoteTool) IsLongRunning() bool { return false }

func (t *memoryClearNoteTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: "OBJECT",
			Properties: map[string]*genai.Schema{
				"id": {Type: "STRING", Description: "Note ID returned by memory_note."},
			},
			Required: []string{"id"},
		},
	}
}

func (t *memoryClearNoteTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *memoryClearNoteTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	m, ok := args.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("memory_clear_note: unexpected args: %T", args)
	}
	id, _ := m["id"].(string)
	if id == "" {
		return nil, fmt.Errorf("memory_clear_note: missing required parameter: id")
	}
	sid := ctx.SessionID()
	if sid == "" {
		return nil, fmt.Errorf("memory_clear_note: no session id in tool context")
	}
	affected, err := t.sessions.DeleteNoteAsAuthor(ctx, id, sid)
	if err != nil {
		return nil, fmt.Errorf("memory_clear_note: %w", err)
	}
	if affected == 0 {
		// Could be missing row OR author mismatch. GetNote disambiguates
		// for a useful error surface; housekeeper paths (reviewer etc.)
		// bypass this tool and call the store directly, so the
		// distinction is LLM-facing only.
		existing, lookupErr := t.sessions.GetNote(ctx, id)
		switch {
		case lookupErr == nil && existing == nil:
			return nil, fmt.Errorf("memory_clear_note: note %q not found", id)
		case lookupErr == nil && existing.AuthorSessionID == "":
			return nil, fmt.Errorf("memory_clear_note: not authorised to clear note %q (legacy note with no author)", id)
		default:
			return nil, fmt.Errorf("memory_clear_note: not authorised to clear note %q", id)
		}
	}
	// Invalidate this session + (best effort) the session the note
	// lived in. We don't have an efficient way to reach the target
	// session here without another hub round-trip; the 10 s TTL on
	// cached note blocks keeps everyone else eventually-consistent.
	if sess, err := t.sm.Session(sid); err == nil {
		sess.InvalidateNotesCache()
	}
	return map[string]any{"cleared": true, "id": id}, nil
}
