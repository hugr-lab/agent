// Hypothesis + session_reviews + memory_log GraphQL bindings for
// HubDB.Learning.
//
// Append-only: every state transition is a DELETE + INSERT. The
// shared `logBatch` helper writes multiple memory_log rows with
// µs-offset timestamps so the composite PK
// (event_time, event_type, memory_item_id, session_id) never collides
// within a single call (spec 005 research Decision 3).
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/pkg/id"
)

// ------------------------------------------------------------
// Hypotheses
// ------------------------------------------------------------

// CreateHypothesis inserts a new hypothesis row and returns its ID.
// If the caller supplied no ID, the adapter generates one via
// pkg/id.New(PrefixHypothesis, ...).
func (h *hubDB) CreateHypothesis(ctx context.Context, hyp Hypothesis) (string, error) {
	if hyp.Content == "" {
		return "", fmt.Errorf("hubdb: CreateHypothesis requires Content")
	}
	if hyp.ID == "" {
		hyp.ID = id.New(id.PrefixHypothesis, h.agentShort)
	}
	if hyp.AgentID == "" {
		hyp.AgentID = h.agentID
	}
	if hyp.Status == "" {
		hyp.Status = "proposed"
	}
	if hyp.Priority == "" {
		hyp.Priority = "medium"
	}
	data := map[string]any{
		"id":              hyp.ID,
		"agent_id":        hyp.AgentID,
		"content":         hyp.Content,
		"category":        hyp.Category,
		"status":          hyp.Status,
		"priority":        hyp.Priority,
		"verification":    hyp.Verification,
		"estimated_calls": hyp.EstimatedCalls,
	}
	if hyp.SourceSession != "" {
		data["source_session_id"] = hyp.SourceSession
	}
	if err := runMutation(ctx, h.querier,
		`mutation ($data: hub_db_hypotheses_mut_input_data!) {
			hub { db { agent {
				insert_hypotheses(data: $data) { id }
			}}}
		}`,
		map[string]any{"data": data},
	); err != nil {
		return "", err
	}
	return hyp.ID, nil
}

// ListPendingHypotheses returns proposed hypotheses, optionally filtered
// by priority, ordered oldest-first to keep scheduler fairness.
func (h *hubDB) ListPendingHypotheses(ctx context.Context, priority string, limit int) ([]Hypothesis, error) {
	if limit <= 0 {
		limit = 10
	}
	filter := map[string]any{
		"agent_id": map[string]any{"eq": h.agentID},
		"status":   map[string]any{"eq": "proposed"},
	}
	if priority != "" {
		filter["priority"] = map[string]any{"eq": priority}
	}
	type row struct {
		ID             string `json:"id"`
		AgentID        string `json:"agent_id"`
		Content        string `json:"content"`
		Category       string `json:"category"`
		Status         string `json:"status"`
		Priority       string `json:"priority"`
		Verification   string `json:"verification"`
		EstimatedCalls int    `json:"estimated_calls"`
		SourceSession  string `json:"source_session_id"`
		CreatedAt      dbTime `json:"created_at"`
	}
	rows, err := runQuery[[]row](ctx, h.querier,
		`query ($filter: hub_db_hypotheses_filter, $limit: Int!) {
			hub { db { agent {
				hypotheses(filter: $filter, limit: $limit, order_by: [{field: "created_at", direction: ASC}]) {
					id agent_id content category status priority verification
					estimated_calls source_session_id created_at
				}
			}}}
		}`,
		map[string]any{"filter": filter, "limit": limit},
		"hub.db.agent.hypotheses",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Hypothesis, 0, len(rows))
	for _, r := range rows {
		out = append(out, Hypothesis{
			ID:             r.ID,
			AgentID:        r.AgentID,
			Content:        r.Content,
			Category:       r.Category,
			Status:         r.Status,
			Priority:       r.Priority,
			Verification:   r.Verification,
			EstimatedCalls: r.EstimatedCalls,
			SourceSession:  r.SourceSession,
			CreatedAt:      r.CreatedAt.Time,
		})
	}
	return out, nil
}

// hypothesisReplace is the common DELETE + INSERT pattern for state
// transitions on hypotheses (append-only). Takes the current row and a
// callback that mutates a copy; writes the new row back.
func (h *hubDB) hypothesisReplace(ctx context.Context, hypID string, mutate func(*Hypothesis)) error {
	current, err := h.getHypothesis(ctx, hypID)
	if err != nil {
		return err
	}
	if current == nil {
		return fmt.Errorf("hubdb: hypothesis %q not found", hypID)
	}
	mutate(current)
	if err := runMutation(ctx, h.querier,
		`mutation ($id: String!) {
			hub { db { agent {
				delete_hypotheses(filter: {id: {eq: $id}}) { affected_rows }
			}}}
		}`,
		map[string]any{"id": hypID},
	); err != nil {
		return err
	}
	data := map[string]any{
		"id":              current.ID,
		"agent_id":        current.AgentID,
		"content":         current.Content,
		"category":        current.Category,
		"status":          current.Status,
		"priority":        current.Priority,
		"verification":    current.Verification,
		"estimated_calls": current.EstimatedCalls,
	}
	if current.SourceSession != "" {
		data["source_session_id"] = current.SourceSession
	}
	if current.CheckedAt != nil {
		data["checked_at"] = current.CheckedAt.UTC().Format(time.RFC3339)
	}
	if current.Result != "" {
		data["result"] = current.Result
	}
	if current.FactID != "" {
		data["fact_id"] = current.FactID
	}
	return runMutation(ctx, h.querier,
		`mutation ($data: hub_db_hypotheses_mut_input_data!) {
			hub { db { agent {
				insert_hypotheses(data: $data) { id }
			}}}
		}`,
		map[string]any{"data": data},
	)
}

func (h *hubDB) getHypothesis(ctx context.Context, hypID string) (*Hypothesis, error) {
	type row struct {
		ID             string  `json:"id"`
		AgentID        string  `json:"agent_id"`
		Content        string  `json:"content"`
		Category       string  `json:"category"`
		Status         string  `json:"status"`
		Priority       string  `json:"priority"`
		Verification   string  `json:"verification"`
		EstimatedCalls int     `json:"estimated_calls"`
		SourceSession  string  `json:"source_session_id"`
		CreatedAt      dbTime  `json:"created_at"`
		CheckedAt      *dbTime `json:"checked_at"`
		Result         string  `json:"result"`
		FactID         string  `json:"fact_id"`
	}
	rows, err := runQuery[[]row](ctx, h.querier,
		`query ($agent: String!, $id: String!) {
			hub { db { agent {
				hypotheses(filter: {agent_id: {eq: $agent}, id: {eq: $id}}, limit: 1) {
					id agent_id content category status priority verification
					estimated_calls source_session_id created_at checked_at result fact_id
				}
			}}}
		}`,
		map[string]any{"agent": h.agentID, "id": hypID},
		"hub.db.agent.hypotheses",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	r := rows[0]
	out := &Hypothesis{
		ID:             r.ID,
		AgentID:        r.AgentID,
		Content:        r.Content,
		Category:       r.Category,
		Status:         r.Status,
		Priority:       r.Priority,
		Verification:   r.Verification,
		EstimatedCalls: r.EstimatedCalls,
		SourceSession:  r.SourceSession,
		CreatedAt:      r.CreatedAt.Time,
		Result:         r.Result,
		FactID:         r.FactID,
	}
	if r.CheckedAt != nil && !r.CheckedAt.Time.IsZero() {
		ct := r.CheckedAt.Time
		out.CheckedAt = &ct
	}
	return out, nil
}

// MarkHypothesisChecking flips status → checking and stamps checked_at.
func (h *hubDB) MarkHypothesisChecking(ctx context.Context, hypID string) error {
	now := time.Now().UTC()
	return h.hypothesisReplace(ctx, hypID, func(h *Hypothesis) {
		h.Status = "checking"
		h.CheckedAt = &now
	})
}

// ConfirmHypothesis marks the hypothesis confirmed with evidence +
// links to the fact produced from it.
func (h *hubDB) ConfirmHypothesis(ctx context.Context, hypID string, evidence, factID string) error {
	now := time.Now().UTC()
	return h.hypothesisReplace(ctx, hypID, func(h *Hypothesis) {
		h.Status = "confirmed"
		h.Result = evidence
		h.FactID = factID
		h.CheckedAt = &now
	})
}

// RejectHypothesis marks the hypothesis rejected with evidence.
func (h *hubDB) RejectHypothesis(ctx context.Context, hypID string, evidence string) error {
	now := time.Now().UTC()
	return h.hypothesisReplace(ctx, hypID, func(h *Hypothesis) {
		h.Status = "rejected"
		h.Result = evidence
		h.CheckedAt = &now
	})
}

// DeferHypothesis puts a checking-state hypothesis back to proposed so
// the scheduler retries later.
func (h *hubDB) DeferHypothesis(ctx context.Context, hypID string) error {
	return h.hypothesisReplace(ctx, hypID, func(h *Hypothesis) {
		h.Status = "proposed"
	})
}

// ExpireOldHypotheses deletes hypotheses older than maxAge that never
// advanced past `proposed`. Returns the affected_rows count.
func (h *hubDB) ExpireOldHypotheses(ctx context.Context, maxAge time.Duration) (int, error) {
	type result struct {
		Affected int `json:"affected_rows"`
	}
	cutoff := time.Now().UTC().Add(-maxAge).Format(time.RFC3339)
	resp, err := h.querier.Query(ctx,
		`mutation ($agent: String!, $cutoff: Timestamp!) {
			hub { db { agent {
				delete_hypotheses(filter: {
					agent_id: {eq: $agent}, status: {eq: "proposed"}, created_at: {lt: $cutoff}
				}) { affected_rows }
			}}}
		}`,
		map[string]any{"agent": h.agentID, "cutoff": cutoff},
	)
	if err != nil {
		return 0, fmt.Errorf("hubdb mutation: %w", err)
	}
	defer resp.Close()
	if err := resp.Err(); err != nil {
		return 0, fmt.Errorf("hubdb graphql: %w", err)
	}
	var r result
	if err := resp.ScanData("hub.db.agent.delete_hypotheses", &r); err != nil {
		if !errors.Is(err, types.ErrWrongDataPath) && !errors.Is(err, types.ErrNoData) {
			return 0, fmt.Errorf("hubdb scan: %w", err)
		}
	}
	return r.Affected, nil
}

// ------------------------------------------------------------
// Session reviews
// ------------------------------------------------------------

// CreateReview inserts a new session_reviews row. If a row for this
// session already exists, returns its existing ID — idempotent.
func (h *hubDB) CreateReview(ctx context.Context, r SessionReview) (string, error) {
	existing, err := h.GetReview(ctx, r.SessionID)
	if err != nil {
		return "", err
	}
	if existing != nil {
		return existing.ID, nil
	}
	if r.ID == "" {
		r.ID = id.New(id.PrefixReview, h.agentShort)
	}
	if r.AgentID == "" {
		r.AgentID = h.agentID
	}
	if r.Status == "" {
		r.Status = "pending"
	}
	data := map[string]any{
		"id":               r.ID,
		"agent_id":         r.AgentID,
		"session_id":       r.SessionID,
		"status":           r.Status,
		"facts_stored":     r.FactsStored,
		"facts_reinforced": r.FactsReinforced,
		"hypotheses_added": r.HypothesesAdded,
		"tokens_used":      r.TokensUsed,
	}
	if r.ModelUsed != "" {
		data["model_used"] = r.ModelUsed
	}
	if err := runMutation(ctx, h.querier,
		`mutation ($data: hub_db_session_reviews_mut_input_data!) {
			hub { db { agent {
				insert_session_reviews(data: $data) { id }
			}}}
		}`,
		map[string]any{"data": data},
	); err != nil {
		return "", err
	}
	return r.ID, nil
}

// GetReview returns the review row for a session, or (nil, nil) when
// none exists.
func (h *hubDB) GetReview(ctx context.Context, sessionID string) (*SessionReview, error) {
	rows, err := h.listReviews(ctx, map[string]any{
		"agent_id":   map[string]any{"eq": h.agentID},
		"session_id": map[string]any{"eq": sessionID},
	}, 1, "")
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}

// ListPendingReviews returns pending review rows, oldest first, up to
// limit.
func (h *hubDB) ListPendingReviews(ctx context.Context, limit int) ([]SessionReview, error) {
	if limit <= 0 {
		limit = 10
	}
	return h.listReviews(ctx, map[string]any{
		"agent_id": map[string]any{"eq": h.agentID},
		"status":   map[string]any{"eq": "pending"},
	}, limit, "ASC")
}

func (h *hubDB) listReviews(ctx context.Context, filter map[string]any, limit int, direction string) ([]SessionReview, error) {
	type row struct {
		ID              string  `json:"id"`
		AgentID         string  `json:"agent_id"`
		SessionID       string  `json:"session_id"`
		Status          string  `json:"status"`
		FactsStored     int     `json:"facts_stored"`
		FactsReinforced int     `json:"facts_reinforced"`
		HypothesesAdded int     `json:"hypotheses_added"`
		ModelUsed       string  `json:"model_used"`
		TokensUsed      int     `json:"tokens_used"`
		ReviewedAt      *dbTime `json:"reviewed_at"`
		Error           string  `json:"error"`
	}
	q := `query ($filter: hub_db_session_reviews_filter, $limit: Int!) {
			hub { db { agent {
				session_reviews(filter: $filter, limit: $limit) {
					id agent_id session_id status facts_stored facts_reinforced
					hypotheses_added model_used tokens_used reviewed_at error
				}
			}}}
		}`
	if direction != "" {
		q = `query ($filter: hub_db_session_reviews_filter, $limit: Int!) {
				hub { db { agent {
					session_reviews(filter: $filter, limit: $limit, order_by: [{field: "id", direction: ` + direction + `}]) {
						id agent_id session_id status facts_stored facts_reinforced
						hypotheses_added model_used tokens_used reviewed_at error
					}
				}}}
			}`
	}
	rows, err := runQuery[[]row](ctx, h.querier, q, map[string]any{
		"filter": filter,
		"limit":  limit,
	}, "hub.db.agent.session_reviews")
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]SessionReview, 0, len(rows))
	for _, r := range rows {
		rev := SessionReview{
			ID:              r.ID,
			AgentID:         r.AgentID,
			SessionID:       r.SessionID,
			Status:          r.Status,
			FactsStored:     r.FactsStored,
			FactsReinforced: r.FactsReinforced,
			HypothesesAdded: r.HypothesesAdded,
			ModelUsed:       r.ModelUsed,
			TokensUsed:      r.TokensUsed,
			Error:           r.Error,
		}
		if r.ReviewedAt != nil && !r.ReviewedAt.Time.IsZero() {
			t := r.ReviewedAt.Time
			rev.ReviewedAt = &t
		}
		out = append(out, rev)
	}
	return out, nil
}

// CompleteReview transitions a pending review to completed with the
// result metadata filled in. Append-only: delete + insert.
func (h *hubDB) CompleteReview(ctx context.Context, reviewID string, result ReviewResult) error {
	return h.replaceReview(ctx, reviewID, func(r *SessionReview) {
		r.Status = "completed"
		r.FactsStored = result.FactsStored
		r.FactsReinforced = result.FactsReinforced
		r.HypothesesAdded = result.HypothesesAdded
		r.ModelUsed = result.ModelUsed
		r.TokensUsed = result.TokensUsed
		now := time.Now().UTC()
		r.ReviewedAt = &now
	})
}

// FailReview marks a review as failed with an error message.
func (h *hubDB) FailReview(ctx context.Context, reviewID string, errMsg string) error {
	return h.replaceReview(ctx, reviewID, func(r *SessionReview) {
		r.Status = "failed"
		r.Error = errMsg
		now := time.Now().UTC()
		r.ReviewedAt = &now
	})
}

func (h *hubDB) replaceReview(ctx context.Context, reviewID string, mutate func(*SessionReview)) error {
	// Fetch current row by ID (not by session).
	rows, err := h.listReviews(ctx, map[string]any{
		"agent_id": map[string]any{"eq": h.agentID},
		"id":       map[string]any{"eq": reviewID},
	}, 1, "")
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("hubdb: review %q not found", reviewID)
	}
	current := rows[0]
	mutate(&current)
	if err := runMutation(ctx, h.querier,
		`mutation ($id: String!) {
			hub { db { agent {
				delete_session_reviews(filter: {id: {eq: $id}}) { affected_rows }
			}}}
		}`,
		map[string]any{"id": reviewID},
	); err != nil {
		return err
	}
	data := map[string]any{
		"id":               current.ID,
		"agent_id":         current.AgentID,
		"session_id":       current.SessionID,
		"status":           current.Status,
		"facts_stored":     current.FactsStored,
		"facts_reinforced": current.FactsReinforced,
		"hypotheses_added": current.HypothesesAdded,
		"tokens_used":      current.TokensUsed,
	}
	if current.ModelUsed != "" {
		data["model_used"] = current.ModelUsed
	}
	if current.Error != "" {
		data["error"] = current.Error
	}
	if current.ReviewedAt != nil {
		data["reviewed_at"] = current.ReviewedAt.UTC().Format(time.RFC3339)
	}
	return runMutation(ctx, h.querier,
		`mutation ($data: hub_db_session_reviews_mut_input_data!) {
			hub { db { agent {
				insert_session_reviews(data: $data) { id }
			}}}
		}`,
		map[string]any{"data": data},
	)
}

// ------------------------------------------------------------
// Memory log
// ------------------------------------------------------------

// Log inserts a single memory_log row.
func (h *hubDB) Log(ctx context.Context, entry MemoryLogEntry) error {
	return h.logBatch(ctx, []MemoryLogEntry{entry})
}

// logBatch writes multiple memory_log rows as a sequence of single-row
// inserts. Each row gets baseTime + idx*1µs via the caller so the
// composite PK stays collision-free (see memory.go).
//
// session_id is part of the composite primary key AND an FK onto
// sessions — so entries without a session (background consolidation,
// tests seeding facts outside an active session) are silently
// skipped. This means such writes lose their audit trail; accept it
// for now rather than creating a sentinel session row at bootstrap.
// The full loop (session open → writes happen within WithSessionID)
// always carries a session ID and therefore logs normally.
func (h *hubDB) logBatch(ctx context.Context, entries []MemoryLogEntry) error {
	for _, e := range entries {
		if e.SessionID == "" {
			continue // no session → cannot satisfy FK; skip audit row
		}
		if e.AgentID == "" {
			e.AgentID = h.agentID
		}
		if e.EventTime.IsZero() {
			e.EventTime = time.Now().UTC()
		}
		row := map[string]any{
			"event_time":     e.EventTime.UTC().Format("2006-01-02T15:04:05.999999Z07:00"),
			"event_type":     e.EventType,
			"memory_item_id": e.MemoryItemID,
			"agent_id":       e.AgentID,
			"session_id":     e.SessionID,
		}
		if e.Details != nil {
			row["details"] = e.Details
		}
		if err := runMutation(ctx, h.querier,
			`mutation ($data: hub_db_memory_log_mut_input_data!) {
				hub { db { agent {
					insert_memory_log(data: $data) { event_time }
				}}}
			}`,
			map[string]any{"data": row},
		); err != nil {
			return err
		}
	}
	return nil
}

// GetLog returns audit entries for a memory item, most recent first.
func (h *hubDB) GetLog(ctx context.Context, memoryItemID string, limit int) ([]MemoryLogEntry, error) {
	if limit <= 0 {
		limit = 20
	}
	type row struct {
		EventTime    dbTime          `json:"event_time"`
		EventType    string          `json:"event_type"`
		MemoryItemID string          `json:"memory_item_id"`
		SessionID    string          `json:"session_id"`
		AgentID      string          `json:"agent_id"`
		Details      json.RawMessage `json:"details"`
	}
	rows, err := runQuery[[]row](ctx, h.querier,
		`query ($agent: String!, $mid: String!, $limit: Int!) {
			hub { db { agent {
				memory_log(
					filter: {agent_id: {eq: $agent}, memory_item_id: {eq: $mid}}
					limit: $limit
					order_by: [{field: "event_time", direction: DESC}]
				) {
					event_time event_type memory_item_id session_id agent_id details
				}
			}}}
		}`,
		map[string]any{"agent": h.agentID, "mid": memoryItemID, "limit": limit},
		"hub.db.agent.memory_log",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]MemoryLogEntry, 0, len(rows))
	for _, r := range rows {
		var details map[string]any
		if len(r.Details) > 0 {
			_ = json.Unmarshal(r.Details, &details)
		}
		out = append(out, MemoryLogEntry{
			EventTime:    r.EventTime.Time,
			EventType:    r.EventType,
			MemoryItemID: r.MemoryItemID,
			SessionID:    r.SessionID,
			AgentID:      r.AgentID,
			Details:      details,
		})
	}
	return out, nil
}
