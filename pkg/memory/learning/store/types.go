package store

import "time"

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

// Review is a record of a post-session review run (session_reviews row).
type Review struct {
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
