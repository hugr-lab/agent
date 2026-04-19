package interfaces

import (
	"context"
	"time"

	adksession "google.golang.org/adk/session"
	"google.golang.org/adk/tool"
)

// SessionManager owns runtime sessions. It implements ADK's
// session.Service so it can be plugged into Runner, and adds our own
// Session(id) accessor plus RestoreOpen for boot-time replay from hub.db.
//
// A SessionManager is also a tools.Provider (Name()+Tools()) — it exposes
// the built-in system tools (skill_list, skill_load, skill_ref,
// context_status) as a provider to the tools.Manager. We keep that contract
// out of this file to avoid an import cycle with pkg/tools; pkg/session's
// concrete Manager satisfies pkg/tools.Provider structurally.
type SessionManager interface {
	adksession.Service

	// Session returns the runtime Session for the given ID. Returns an error
	// if no session with that ID is currently tracked.
	Session(id string) (Session, error)

	// RestoreOpen loads all sessions whose hub.db status is "active" and
	// replays their skill_loaded/skill_unloaded events into state. Called
	// once at startup.
	RestoreOpen(ctx context.Context) error

	// Cleanup removes sessions inactive for more than olderThan. Returns
	// the number of sessions purged.
	Cleanup(olderThan time.Duration) int
}

// Session is a runtime conversation. It satisfies ADK's session.Session
// contract (ID, AppName, UserID, State, Events, LastUpdateTime) and adds
// the hugr-specific catalogue / skill / reference operations.
type Session interface {
	adksession.Session

	// Snapshot returns the current prompt + tool list. Cached; rebuilt lazily
	// when the session is dirty.
	Snapshot() Snapshot

	// SetCatalog replaces the skill catalogue shown in this session's prompt.
	// Called by skill_list.
	SetCatalog(skills []SkillMeta) error

	// LoadSkill activates a skill for this session: appends it to state.Skills
	// + state.Tools, writes a skill_loaded event to hub.db, invalidates the
	// snapshot cache.
	LoadSkill(ctx context.Context, name string) error

	// UnloadSkill removes a skill from this session and writes
	// skill_unloaded. No-op if the skill is not active.
	UnloadSkill(ctx context.Context, name string) error

	// LoadReference appends a reference document (skill/ref_name) to the
	// prompt extras.
	LoadReference(ctx context.Context, skill, ref string) error

	// UnloadReference removes a previously-loaded reference from the
	// prompt extras. No-op if the reference wasn't loaded.
	UnloadReference(ctx context.Context, skill, ref string) error

	// IngestADKEvent is called from Manager.AppendEvent so the session can
	// classify the event and persist it (conversation-event persistence is
	// implemented in spec 003b; in 004 this is mostly a debug tap).
	IngestADKEvent(ctx context.Context, ev *adksession.Event)
}

// Snapshot is the per-turn view of a session: assembled system prompt plus
// the flat list of tools the LLM should see. Returned by Session.Snapshot()
// and consumed by tools.Inject (BeforeModelCallback) + the agent's
// InstructionProvider.
type Snapshot struct {
	Prompt string
	Tools  []tool.Tool
}

// MCPSpec is the endpoint information needed to stand up an MCP toolset
// for a skill. Produced by skills.Manager.MCPs().
type MCPSpec struct {
	SkillName string
	Endpoint  string
}

// ============================================================
// Persisted session_events metadata
// ============================================================

// Event type constants written to hub.db session_events.event_type.
// In 004 we persist only skill lifecycle events; conversation events
// (user_message, llm_response, tool_call, tool_result) are added in 003b.
const (
	EventTypeSkillLoaded   = "skill_loaded"
	EventTypeSkillUnloaded = "skill_unloaded"

	// The following are reserved names for 003b — keep strings stable so
	// early writers can start producing them before full integration.
	EventTypeUserMessage   = "user_message"
	EventTypeLLMResponse   = "llm_response"
	EventTypeToolCall      = "tool_call"
	EventTypeToolResult    = "tool_result"
	EventTypeSessionForked = "session_forked"
	EventTypeNote          = "note"
	EventTypeError         = "error"
)

// SkillLoadedMeta is the JSON payload of a skill_loaded event's Metadata.
type SkillLoadedMeta struct {
	Skill       string   `json:"skill"`
	MCPEndpoint string   `json:"mcp_endpoint,omitempty"`
	Tools       []string `json:"tools,omitempty"`
	Refs        []string `json:"refs,omitempty"`
}

// SkillUnloadedMeta is the JSON payload of a skill_unloaded event's Metadata.
type SkillUnloadedMeta struct {
	Skill string `json:"skill"`
}

// LLMResponseMeta is the payload of an llm_response event (reserved for 003b).
type LLMResponseMeta struct {
	Model            string `json:"model,omitempty"`
	PromptTokens     int    `json:"prompt_tokens,omitempty"`
	CompletionTokens int    `json:"completion_tokens,omitempty"`
}

// ToolCallMeta is the payload of a tool_call event (reserved for 003b).
type ToolCallMeta struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args,omitempty"`
}

// ToolResultMeta is the payload of a tool_result event (reserved for 003b).
type ToolResultMeta struct {
	Tool     string `json:"tool"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	Duration int64  `json:"duration_ms,omitempty"`
}
