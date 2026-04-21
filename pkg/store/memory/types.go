package memory

import "time"

// Item is one persisted fact (memory_items row).
type Item struct {
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

// Link is a directed relation between two memory items.
type Link struct {
	SourceID  string     `json:"source_id"`
	TargetID  string     `json:"target_id"`
	Relation  string     `json:"relation"`
	CreatedAt time.Time  `json:"created_at"`
	ValidTo   *time.Time `json:"valid_to,omitempty"`
}

// SearchOpts filters Client.Search results.
type SearchOpts struct {
	Category  string
	Tags      []string
	Limit     int
	MinScore  float64
	ValidAt   *time.Time
	ValidOnly bool
}

// SearchResult is one row returned by Client.Search / GetLinked.
type SearchResult struct {
	Item
	IsValid       bool     `json:"is_valid"`
	AgeDays       int      `json:"age_days"`
	ExpiresInDays int      `json:"expires_in_days"`
	Tags          []string `json:"tags"`
	Links         []string `json:"links"`
	Distance      float64  `json:"distance"`
}

// Stats is the aggregate view returned by Client.Stats.
type Stats struct {
	TotalItems    int            `json:"total_items"`
	ActiveItems   int            `json:"active_items"`
	ByCategory    map[string]int `json:"by_category"`
	OldestFact    time.Time      `json:"oldest_fact"`
	NewestFact    time.Time      `json:"newest_fact"`
	TotalTags     int            `json:"total_tags"`
	TotalLinks    int            `json:"total_links"`
	PendingReview int            `json:"pending_review"`
}

// LogEntry is one audit-log row describing a memory mutation (memory_log).
type LogEntry struct {
	EventTime    time.Time      `json:"event_time"`
	EventType    string         `json:"event_type"`
	MemoryItemID string         `json:"memory_item_id"`
	SessionID    string         `json:"session_id"`
	AgentID      string         `json:"agent_id"`
	Details      map[string]any `json:"details"`
}
