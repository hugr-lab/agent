package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/pkg/store/queries"
)

// Log inserts a single memory_log row.
func (c *Client) Log(ctx context.Context, entry LogEntry) error {
	return c.logBatch(ctx, []LogEntry{entry})
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
func (c *Client) logBatch(ctx context.Context, entries []LogEntry) error {
	for _, e := range entries {
		if e.SessionID == "" {
			continue // no session → cannot satisfy FK; skip audit row
		}
		if e.AgentID == "" {
			e.AgentID = c.agentID
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
		if err := queries.RunMutation(ctx, c.querier,
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
func (c *Client) GetLog(ctx context.Context, memoryItemID string, limit int) ([]LogEntry, error) {
	if limit <= 0 {
		limit = 20
	}
	type row struct {
		EventTime    time.Time       `json:"event_time"`
		EventType    string          `json:"event_type"`
		MemoryItemID string          `json:"memory_item_id"`
		SessionID    string          `json:"session_id"`
		AgentID      string          `json:"agent_id"`
		Details      json.RawMessage `json:"details"`
	}
	rows, err := queries.RunQuery[[]row](ctx, c.querier,
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
		map[string]any{"agent": c.agentID, "mid": memoryItemID, "limit": limit},
		"hub.db.agent.memory_log",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]LogEntry, 0, len(rows))
	for _, r := range rows {
		var details map[string]any
		if len(r.Details) > 0 {
			_ = json.Unmarshal(r.Details, &details)
		}
		out = append(out, LogEntry{
			EventTime:    r.EventTime,
			EventType:    r.EventType,
			MemoryItemID: r.MemoryItemID,
			SessionID:    r.SessionID,
			AgentID:      r.AgentID,
			Details:      details,
		})
	}
	return out, nil
}
