package sessions

import "time"

// Record is the persisted session row (hub.db.sessions).
type Record struct {
	ID              string         `json:"id"`
	AgentID         string         `json:"agent_id"`
	OwnerID         string         `json:"owner_id"`
	ParentSessionID string         `json:"parent_session_id"`
	ForkAfterSeq    *int           `json:"fork_after_seq,omitempty"`
	Status          string         `json:"status"`
	Mission         string         `json:"mission"`
	Metadata        map[string]any `json:"metadata"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
}

// Event is one row in the session_events append-only log.
type Event struct {
	ID         string         `json:"id"`
	SessionID  string         `json:"session_id"`
	AgentID    string         `json:"agent_id"`
	Seq        int            `json:"seq"`
	EventType  string         `json:"event_type"`
	Author     string         `json:"author"`
	Content    string         `json:"content"`
	ToolName   string         `json:"tool_name"`
	ToolArgs   map[string]any `json:"tool_args"`
	ToolResult string         `json:"tool_result"`
	Metadata   map[string]any `json:"metadata"`
	CreatedAt  time.Time      `json:"created_at"`
}

// EventFull adds chain_depth, set when the event was emitted from a
// sub-session (e.g. hypothesis verifier).
type EventFull struct {
	Event
	ChainDepth int `json:"chain_depth"`
}

// Note is an LLM-authored scratchpad entry attached to a session.
type Note struct {
	ID        string    `json:"id"`
	AgentID   string    `json:"agent_id"`
	SessionID string    `json:"session_id"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// Participant is one user attached to a session.
type Participant struct {
	SessionID string     `json:"session_id"`
	UserID    string     `json:"user_id"`
	Role      string     `json:"role"`
	JoinedAt  time.Time  `json:"joined_at"`
	LeftAt    *time.Time `json:"left_at,omitempty"`
}
