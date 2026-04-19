// Package interfaces declares the Go API surface used by the agent runtime.
package interfaces

import (
	"context"
	"time"
)

// HubDB is the unified agent data access interface. Built on types.Querier —
// same implementation for standalone (embedded hugr) and hub (remote client).
//
// All operations are scoped to the agent's identity. In 004 scope only
// AgentRegistry and Embeddings are implemented; Memory/Learning/Sessions
// are stubbed and return "not implemented" until spec 003b.
type HubDB interface {
	AgentID() string

	AgentRegistry
	Memory
	Learning
	Sessions
	Embeddings

	Close() error
}

// ============================================================
// AgentRegistry
// ============================================================

// AgentRegistry manages agent type configurations and running instances.
type AgentRegistry interface {
	GetAgentType(ctx context.Context, typeID string) (*AgentType, error)
	UpsertAgentType(ctx context.Context, at AgentType) error
	GetAgent(ctx context.Context, id string) (*Agent, error)
	RegisterAgent(ctx context.Context, a Agent) error
	UpdateAgentActivity(ctx context.Context, id string) error
	ListAgents(ctx context.Context, typeID string) ([]Agent, error)
}

type AgentType struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Config      map[string]any `json:"config"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

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

// ============================================================
// Memory
// ============================================================

// Memory manages persistent facts with tags, links, and vector search.
// APPEND-ONLY: Reinforce/Supersede = DELETE + INSERT.
type Memory interface {
	Search(ctx context.Context, query string, embedding []float32, opts SearchOpts) ([]SearchResult, error)
	Get(ctx context.Context, id string) (*SearchResult, error)
	GetLinked(ctx context.Context, id string, depth int) ([]SearchResult, error)

	Store(ctx context.Context, item MemoryItem, tags []string, links []MemoryLink) (string, error)
	Reinforce(ctx context.Context, id string, scoreBonus float64, extraTags []string, extraLinks []MemoryLink) error
	Supersede(ctx context.Context, oldID string, newItem MemoryItem, tags []string, links []MemoryLink) (string, error)
	Delete(ctx context.Context, id string) error
	DeleteExpired(ctx context.Context) (int, error)

	AddTags(ctx context.Context, memoryItemID string, tags []string) error
	RemoveTags(ctx context.Context, memoryItemID string, tags []string) error

	AddLink(ctx context.Context, link MemoryLink) error
	RemoveLink(ctx context.Context, sourceID, targetID string) error

	Stats(ctx context.Context) (MemoryStats, error)
	Hint(ctx context.Context, query string, embedding []float32) (string, error)
}

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

type MemoryLink struct {
	SourceID  string     `json:"source_id"`
	TargetID  string     `json:"target_id"`
	Relation  string     `json:"relation"`
	CreatedAt time.Time  `json:"created_at"`
	ValidTo   *time.Time `json:"valid_to,omitempty"`
}

type SearchOpts struct {
	Category  string
	Tags      []string
	Limit     int
	MinScore  float64
	ValidAt   *time.Time
	ValidOnly bool
}

type SearchResult struct {
	MemoryItem
	IsValid       bool     `json:"is_valid"`
	AgeDays       int      `json:"age_days"`
	ExpiresInDays int      `json:"expires_in_days"`
	Tags          []string `json:"tags"`
	Links         []string `json:"links"`
	Distance      float64  `json:"distance"`
}

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

// ============================================================
// Learning
// ============================================================

// Learning manages hypotheses, reviews, and the memory audit log.
type Learning interface {
	CreateHypothesis(ctx context.Context, h Hypothesis) (string, error)
	ListPendingHypotheses(ctx context.Context, priority string, limit int) ([]Hypothesis, error)
	MarkHypothesisChecking(ctx context.Context, id string) error
	ConfirmHypothesis(ctx context.Context, id string, evidence, factID string) error
	RejectHypothesis(ctx context.Context, id string, evidence string) error
	DeferHypothesis(ctx context.Context, id string) error
	ExpireOldHypotheses(ctx context.Context, maxAge time.Duration) (int, error)

	CreateReview(ctx context.Context, review SessionReview) (string, error)
	GetReview(ctx context.Context, sessionID string) (*SessionReview, error)
	ListPendingReviews(ctx context.Context, limit int) ([]SessionReview, error)
	CompleteReview(ctx context.Context, id string, result ReviewResult) error
	FailReview(ctx context.Context, id string, errMsg string) error

	Log(ctx context.Context, entry MemoryLogEntry) error
	GetLog(ctx context.Context, memoryItemID string, limit int) ([]MemoryLogEntry, error)
}

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

type ReviewResult struct {
	FactsStored     int
	FactsReinforced int
	HypothesesAdded int
	ModelUsed       string
	TokensUsed      int
}

type MemoryLogEntry struct {
	EventTime    time.Time      `json:"event_time"`
	EventType    string         `json:"event_type"`
	MemoryItemID string         `json:"memory_item_id"`
	SessionID    string         `json:"session_id"`
	AgentID      string         `json:"agent_id"`
	Details      map[string]any `json:"details"`
}

// ============================================================
// Sessions
// ============================================================

// Sessions manages conversation sessions, events, notes, and participants.
type Sessions interface {
	CreateSession(ctx context.Context, s SessionRecord) (string, error)
	GetSession(ctx context.Context, id string) (*SessionRecord, error)
	ListActiveSessions(ctx context.Context) ([]SessionRecord, error)
	ListChildSessions(ctx context.Context, parentSessionID string) ([]SessionRecord, error)
	UpdateSessionStatus(ctx context.Context, id, status string) error

	AppendEvent(ctx context.Context, event SessionEvent) (string, error)
	GetEvents(ctx context.Context, sessionID string) ([]SessionEvent, error)
	GetEventsFull(ctx context.Context, sessionID string) ([]SessionEventFull, error)
	CountToolCalls(ctx context.Context, sessionID string) (int, error)

	AddNote(ctx context.Context, note SessionNote) (string, error)
	ListNotes(ctx context.Context, sessionID string) ([]SessionNote, error)
	DeleteNote(ctx context.Context, id string) error
	DeleteSessionNotes(ctx context.Context, sessionID string) (int, error)

	AddParticipant(ctx context.Context, p SessionParticipant) error
	RemoveParticipant(ctx context.Context, sessionID, userID string) error
	ListParticipants(ctx context.Context, sessionID string) ([]SessionParticipant, error)
}

// SessionRecord is the persisted session row (hub.db sessions table).
// The runtime Session interface lives in interfaces/session.go.
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

type SessionEventFull struct {
	SessionEvent
	ChainDepth int `json:"chain_depth"`
}

type SessionNote struct {
	ID        string    `json:"id"`
	AgentID   string    `json:"agent_id"`
	SessionID string    `json:"session_id"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

type SessionParticipant struct {
	SessionID string     `json:"session_id"`
	UserID    string     `json:"user_id"`
	Role      string     `json:"role"`
	JoinedAt  time.Time  `json:"joined_at"`
	LeftAt    *time.Time `json:"left_at,omitempty"`
}

// ============================================================
// Embeddings
// ============================================================

// Embeddings wraps the embedding model registered in the engine.
type Embeddings interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
	Dimension() int
	Available() bool
}
