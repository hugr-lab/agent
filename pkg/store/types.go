package store

import "time"

// AgentType describes one kind of agent (e.g. "hugr-data") and its
// default configuration. Multiple agents may share the same type.
type AgentType struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Config      map[string]any `json:"config"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

// Agent is a running agent instance.
type Agent struct {
	ID             string         `json:"id"`
	AgentTypeID    string         `json:"agent_type_id"`
	ShortID        string         `json:"short_id"`
	Name           string         `json:"name"`
	Status         string         `json:"status"`
	ConfigOverride map[string]any `json:"config_override"`
	CreatedAt      time.Time      `json:"created_at"`
	LastActive     time.Time      `json:"last_active"`
}

// MemoryItem is one persisted fact.
type MemoryItem struct {
	ID         string    `json:"id"`
	AgentID    string    `json:"agent_id"`
	Content    string    `json:"content"`
	Category   string    `json:"category"`
	Volatility string    `json:"volatility"`
	Score      float64   `json:"score"`
	Source     string    `json:"source"`
	ValidFrom  time.Time `json:"valid_from"`
	ValidTo    time.Time `json:"valid_to"`
	CreatedAt  time.Time `json:"created_at"`
}

// MemoryLink is a directed relation between two memory items.
type MemoryLink struct {
	SourceID  string     `json:"source_id"`
	TargetID  string     `json:"target_id"`
	Relation  string     `json:"relation"`
	CreatedAt time.Time  `json:"created_at"`
	ValidTo   *time.Time `json:"valid_to,omitempty"`
}

// SearchOpts filters Memory.Search results.
type SearchOpts struct {
	Category  string
	Tags      []string
	Limit     int
	MinScore  float64
	ValidAt   *time.Time
	ValidOnly bool
}

// SearchResult is one row returned by Memory.Search / GetLinked.
type SearchResult struct {
	MemoryItem
	IsValid       bool     `json:"is_valid"`
	AgeDays       int      `json:"age_days"`
	ExpiresInDays int      `json:"expires_in_days"`
	Tags          []string `json:"tags"`
	Links         []string `json:"links"`
	Distance      float64  `json:"distance"`
}

// MemoryStats is the aggregate view returned by Memory.Stats.
type MemoryStats struct {
	TotalItems    int            `json:"total_items"`
	ActiveItems   int            `json:"active_items"`
	ByCategory    map[string]int `json:"by_category"`
	OldestFact    time.Time      `json:"oldest_fact"`
	NewestFact    time.Time      `json:"newest_fact"`
	TotalTags     int            `json:"total_tags"`
	TotalLinks    int            `json:"total_links"`
	PendingReview int            `json:"pending_review"`
}

// Hypothesis is a yet-unverified belief about the data or environment.
type Hypothesis struct {
	ID             string     `json:"id"`
	AgentID        string     `json:"agent_id"`
	Content        string     `json:"content"`
	Category       string     `json:"category"`
	Status         string     `json:"status"`
	Priority       string     `json:"priority"`
	Verification   string     `json:"verification"`
	EstimatedCalls int        `json:"estimated_calls"`
	SourceSession  string     `json:"source_session"`
	CreatedAt      time.Time  `json:"created_at"`
	CheckedAt      *time.Time `json:"checked_at,omitempty"`
	Result         string     `json:"result"`
	FactID         string     `json:"fact_id"`
}

// SessionReview is a record of a post-session review run.
type SessionReview struct {
	ID              string     `json:"id"`
	AgentID         string     `json:"agent_id"`
	SessionID       string     `json:"session_id"`
	Status          string     `json:"status"`
	FactsStored     int        `json:"facts_stored"`
	FactsReinforced int        `json:"facts_reinforced"`
	HypothesesAdded int        `json:"hypotheses_added"`
	ModelUsed       string     `json:"model_used"`
	TokensUsed      int        `json:"tokens_used"`
	ReviewedAt      *time.Time `json:"reviewed_at,omitempty"`
	Error           string     `json:"error"`
}

// ReviewResult is what a reviewer returns when it completes a review.
type ReviewResult struct {
	FactsStored     int
	FactsReinforced int
	HypothesesAdded int
	ModelUsed       string
	TokensUsed      int
}

// MemoryLogEntry is one audit-log row describing a Memory mutation.
type MemoryLogEntry struct {
	EventTime    time.Time      `json:"event_time"`
	EventType    string         `json:"event_type"`
	MemoryItemID string         `json:"memory_item_id"`
	SessionID    string         `json:"session_id"`
	AgentID      string         `json:"agent_id"`
	Details      map[string]any `json:"details"`
}

// SessionRecord is the persisted session row.
type SessionRecord struct {
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

// SessionEvent is one row in the session_events append-only log.
type SessionEvent struct {
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

// SessionEventFull adds chain_depth, set when the event was emitted from
// a sub-session (e.g. hypothesis verifier).
type SessionEventFull struct {
	SessionEvent
	ChainDepth int `json:"chain_depth"`
}

// SessionNote is an LLM-authored scratchpad entry attached to a session.
type SessionNote struct {
	ID        string    `json:"id"`
	AgentID   string    `json:"agent_id"`
	SessionID string    `json:"session_id"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// SessionParticipant is one user attached to a session.
type SessionParticipant struct {
	SessionID string     `json:"session_id"`
	UserID    string     `json:"user_id"`
	Role      string     `json:"role"`
	JoinedAt  time.Time  `json:"joined_at"`
	LeftAt    *time.Time `json:"left_at,omitempty"`
}
