package store

// Event type constants written to hub.db session_events.event_type.
const (
	EventTypeSkillLoaded   = "skill_loaded"
	EventTypeSkillUnloaded = "skill_unloaded"

	EventTypeUserMessage       = "user_message"
	EventTypeLLMResponse       = "llm_response"
	EventTypeAgentMessage      = "agent_message"
	EventTypeToolCall          = "tool_call"
	EventTypeToolResult        = "tool_result"
	EventTypeSessionForked     = "session_forked"
	EventTypeCompactionSummary = "compaction_summary"
	EventTypeNote              = "note"
	EventTypeError             = "error"

	// Mission lifecycle events (spec 007). Emitted on the coordinator
	// session; excluded from the reviewer pipeline — lifecycle audit,
	// not learning material. See contracts/events.md for payload shapes.
	EventTypeAgentSpawn         = "agent_spawn"
	EventTypeAgentResult        = "agent_result"
	EventTypeAgentAbstained     = "agent_abstained"
	EventTypeUserFollowupRouted = "user_followup_routed"
)

// AgentSpawnMeta is the payload of an agent_spawn event emitted on the
// coordinator session when a mission starts.
type AgentSpawnMeta struct {
	MissionID string `json:"mission_id"`
	Skill     string `json:"skill"`
	Role      string `json:"role"`
	Task      string `json:"task"`
}

// AgentResultMeta is the payload of an agent_result event emitted on
// the coordinator session at a mission's terminal transition. Summary
// is the text that gets embedded server-side via `summary:` — phase-4
// semantic search finds missions by their outcome.
type AgentResultMeta struct {
	MissionID  string `json:"mission_id"`
	Status     string `json:"status"` // completed | failed | abandoned
	TurnsUsed  int    `json:"turns_used"`
	Summary    string `json:"summary"`
	DurationMs int64  `json:"duration_ms,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// AgentAbstainedMeta is the payload of an agent_abstained event emitted
// (in addition to agent_result) when a sub-agent's final message was a
// refusal.
type AgentAbstainedMeta struct {
	MissionID string `json:"mission_id"`
	Reason    string `json:"reason"`
}

// UserFollowupRoutedMeta is the payload of a user_followup_routed audit
// event emitted on the coordinator when FollowUpRouter reroutes a user
// message into a running mission's session.
type UserFollowupRoutedMeta struct {
	TargetMissionID      string  `json:"target_mission_id"`
	ClassifierConfidence float64 `json:"classifier_confidence"`
}

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

// LLMResponseMeta is the payload of an llm_response event.
type LLMResponseMeta struct {
	Model            string `json:"model,omitempty"`
	PromptTokens     int    `json:"prompt_tokens,omitempty"`
	CompletionTokens int    `json:"completion_tokens,omitempty"`
}

// ToolCallMeta is the payload of a tool_call event.
type ToolCallMeta struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args,omitempty"`
}

// ToolResultMeta is the payload of a tool_result event.
type ToolResultMeta struct {
	Tool     string `json:"tool"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	Duration int64  `json:"duration_ms,omitempty"`
}
