package search

import (
	"fmt"
	"time"
)

// Scope selects the corpus axis for session_context. See
// contracts/search-tools.md for the per-scope authorization matrix
// and contracts/search-corpus.md for resolution rules.
type Scope string

const (
	// ScopeTurn returns the caller's last `last_n` events as a
	// chronological tail; rejects a `query` argument.
	ScopeTurn Scope = "turn"
	// ScopeMission searches the caller's own session, ranked by
	// relevance × recency.
	ScopeMission Scope = "mission"
	// ScopeSession searches the coordinator root + every recursive
	// child mission session via session_events_chain. Coordinator-only.
	ScopeSession Scope = "session"
	// ScopeUser searches every root session of the same owner across
	// all time (or the supplied date window). Coordinator-only.
	ScopeUser Scope = "user"
)

// Validate reports whether s is one of the recognised values.
func (s Scope) Validate() error {
	switch s {
	case ScopeTurn, ScopeMission, ScopeSession, ScopeUser:
		return nil
	default:
		return fmt.Errorf("search: invalid scope %q (want turn|mission|session|user)", s)
	}
}

// IsCoordinatorOnly reports whether s requires a coordinator session.
// Sub-agents calling these scopes get ErrScopeForbidden.
func (s Scope) IsCoordinatorOnly() bool {
	return s == ScopeSession || s == ScopeUser
}

// Strategy reports which backend served the search result. Surfaced
// via SessionContextResult for debuggability — not consumed by the
// LLM's answer.
type Strategy string

const (
	// StrategySemantic — Hugr's `semantic:` argument was used and
	// returned non-zero rows.
	StrategySemantic Strategy = "semantic"
	// StrategyKeyword — the corpus had no embedded rows matching the
	// query; the tool fell back to content ILIKE.
	StrategyKeyword Strategy = "keyword"
	// StrategyRecency — caller passed no `query`; results ordered by
	// recency boost only.
	StrategyRecency Strategy = "recency"
)

// SearchHit is one row of a session_context result.
type SearchHit struct {
	ID            string                 `json:"id"`
	SessionID     string                 `json:"session_id"`
	SessionKind   string                 `json:"session_kind"` // "root" | "subagent"
	Seq           int64                  `json:"seq"`
	CreatedAt     time.Time              `json:"created_at"`
	EventType     string                 `json:"event_type"`
	ToolName      string                 `json:"tool_name,omitempty"`
	Author        string                 `json:"author,omitempty"`
	ContentExcerpt string                `json:"content_excerpt"`
	Distance      *float64               `json:"distance,omitempty"`        // nil for keyword/recency
	RecencyBoost  float64                `json:"recency_boost"`
	Combined      float64                `json:"combined"` // (1 - distance) × recency_boost; sort key
	Metadata      map[string]any         `json:"metadata,omitempty"`
}

// SessionContextResult is the result envelope for session_context.
type SessionContextResult struct {
	Hits             []SearchHit `json:"hits"`
	Strategy         Strategy    `json:"strategy"`
	TotalCorpusSize  int64       `json:"total_corpus_size"`
	Scope            Scope       `json:"scope"`
	HalfLife         string      `json:"half_life,omitempty"`
}

// SessionEventsResult is the result envelope for session_events
// (raw audit access; no ranking).
type SessionEventsResult struct {
	Events []SearchHit `json:"events"`
}
