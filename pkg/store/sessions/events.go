package sessions

// Event type constants written to hub.db session_events.event_type.
const (
	EventTypeSkillLoaded   = "skill_loaded"
	EventTypeSkillUnloaded = "skill_unloaded"

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
