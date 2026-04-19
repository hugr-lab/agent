package hubdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/interfaces"
	"github.com/hugr-lab/hugen/pkg/id"
)

// CreateSession inserts a new row into hub.db.agent.sessions. Idempotent:
// if a row with the same ID already exists, returns that ID.
func (h *hubDB) CreateSession(ctx context.Context, s interfaces.SessionRecord) (string, error) {
	if s.ID == "" {
		return "", fmt.Errorf("hubdb: CreateSession requires ID")
	}
	if s.AgentID == "" {
		s.AgentID = h.agentID
	}
	status := s.Status
	if status == "" {
		status = "active"
	}
	data := map[string]any{
		"id":       s.ID,
		"agent_id": s.AgentID,
		"status":   status,
	}
	if s.OwnerID != "" {
		data["owner_id"] = s.OwnerID
	}
	if s.ParentSessionID != "" {
		data["parent_session_id"] = s.ParentSessionID
	}
	if s.ForkAfterSeq != nil {
		data["fork_after_seq"] = *s.ForkAfterSeq
	}
	if s.Mission != "" {
		data["mission"] = s.Mission
	}
	if s.Metadata != nil {
		data["metadata"] = s.Metadata
	}
	err := runMutation(ctx, h.querier,
		`mutation ($data: hub_db_sessions_mut_input_data!) {
			hub { db { agent {
				insert_sessions(data: $data) { id }
			}}}
		}`,
		map[string]any{"data": data},
	)
	if err != nil {
		return "", err
	}
	return s.ID, nil
}

// UpdateSessionStatus updates a session row's status column.
func (h *hubDB) UpdateSessionStatus(ctx context.Context, id, status string) error {
	return runMutation(ctx, h.querier,
		`mutation ($id: String!, $data: hub_db_sessions_mut_data!) {
			hub { db { agent {
				update_sessions(filter: {id: {eq: $id}}, data: $data) { affected_rows }
			}}}
		}`,
		map[string]any{
			"id":   id,
			"data": map[string]any{"status": status},
		},
	)
}

// ListActiveSessions returns all sessions owned by this agent with
// status="active". Used by SessionManager.RestoreOpen on startup.
func (h *hubDB) ListActiveSessions(ctx context.Context) ([]interfaces.SessionRecord, error) {
	type row struct {
		ID              string         `json:"id"`
		AgentID         string         `json:"agent_id"`
		OwnerID         string         `json:"owner_id"`
		ParentSessionID string         `json:"parent_session_id"`
		ForkAfterSeq    *int           `json:"fork_after_seq"`
		Status          string         `json:"status"`
		Mission         string         `json:"mission"`
		Metadata        map[string]any `json:"metadata"`
		CreatedAt       dbTime         `json:"created_at"`
		UpdatedAt       dbTime         `json:"updated_at"`
	}
	rows, err := runQuery[[]row](ctx, h.querier,
		`query ($agent: String!) {
			hub { db { agent {
				sessions(filter: {agent_id: {eq: $agent}, status: {eq: "active"}}) {
					id agent_id owner_id parent_session_id fork_after_seq status mission metadata created_at updated_at
				}
			}}}
		}`,
		map[string]any{"agent": h.agentID},
		"hub.db.agent.sessions",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]interfaces.SessionRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, interfaces.SessionRecord{
			ID:              r.ID,
			AgentID:         r.AgentID,
			OwnerID:         r.OwnerID,
			ParentSessionID: r.ParentSessionID,
			ForkAfterSeq:    r.ForkAfterSeq,
			Status:          r.Status,
			Mission:         r.Mission,
			Metadata:        r.Metadata,
			CreatedAt:       r.CreatedAt.Time,
			UpdatedAt:       r.UpdatedAt.Time,
		})
	}
	return out, nil
}

// AppendEvent inserts a row into hub.db.agent.session_events. Computes seq
// as max(seq)+1 within the session (not transactionally atomic; fine for
// single-writer-per-session which is how ADK drives sessions).
func (h *hubDB) AppendEvent(ctx context.Context, ev interfaces.SessionEvent) (string, error) {
	if ev.SessionID == "" {
		return "", fmt.Errorf("hubdb: AppendEvent requires SessionID")
	}
	if ev.EventType == "" {
		return "", fmt.Errorf("hubdb: AppendEvent requires EventType")
	}
	if ev.AgentID == "" {
		ev.AgentID = h.agentID
	}
	seq := ev.Seq
	if seq == 0 {
		next, err := h.nextSeq(ctx, ev.SessionID)
		if err != nil {
			return "", err
		}
		seq = next
	}
	eventID := ev.ID
	if eventID == "" {
		eventID = id.New(id.PrefixEvent, h.agentShort)
	}
	data := map[string]any{
		"id":         eventID,
		"session_id": ev.SessionID,
		"agent_id":   ev.AgentID,
		"seq":        seq,
		"event_type": ev.EventType,
		"author":     defaultString(ev.Author, ev.AgentID),
	}
	if ev.Content != "" {
		data["content"] = ev.Content
	}
	if ev.ToolName != "" {
		data["tool_name"] = ev.ToolName
	}
	if ev.ToolArgs != nil {
		data["tool_args"] = ev.ToolArgs
	}
	if ev.ToolResult != "" {
		data["tool_result"] = ev.ToolResult
	}
	if ev.Metadata != nil {
		data["metadata"] = ev.Metadata
	}
	if err := runMutation(ctx, h.querier,
		`mutation ($data: hub_db_session_events_mut_input_data!) {
			hub { db { agent {
				insert_session_events(data: $data) { id }
			}}}
		}`,
		map[string]any{"data": data},
	); err != nil {
		return "", err
	}
	return eventID, nil
}

// GetEvents returns every event in a session ordered by seq ASC.
func (h *hubDB) GetEvents(ctx context.Context, sessionID string) ([]interfaces.SessionEvent, error) {
	type row struct {
		ID         string          `json:"id"`
		SessionID  string          `json:"session_id"`
		AgentID    string          `json:"agent_id"`
		Seq        int             `json:"seq"`
		EventType  string          `json:"event_type"`
		Author     string          `json:"author"`
		Content    string          `json:"content"`
		ToolName   string          `json:"tool_name"`
		ToolArgs   json.RawMessage `json:"tool_args"`
		ToolResult string          `json:"tool_result"`
		Metadata   map[string]any  `json:"metadata"`
		CreatedAt  dbTime          `json:"created_at"`
	}
	rows, err := runQuery[[]row](ctx, h.querier,
		`query ($sid: String!) {
			hub { db { agent {
				session_events(filter: {session_id: {eq: $sid}}, order_by: [{field: "seq", direction: ASC}]) {
					id session_id agent_id seq event_type author content tool_name tool_args tool_result metadata created_at
				}
			}}}
		}`,
		map[string]any{"sid": sessionID},
		"hub.db.agent.session_events",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]interfaces.SessionEvent, 0, len(rows))
	for _, r := range rows {
		var toolArgs map[string]any
		if len(r.ToolArgs) > 0 {
			_ = json.Unmarshal(r.ToolArgs, &toolArgs)
		}
		out = append(out, interfaces.SessionEvent{
			ID:         r.ID,
			SessionID:  r.SessionID,
			AgentID:    r.AgentID,
			Seq:        r.Seq,
			EventType:  r.EventType,
			Author:     r.Author,
			Content:    r.Content,
			ToolName:   r.ToolName,
			ToolArgs:   toolArgs,
			ToolResult: r.ToolResult,
			Metadata:   r.Metadata,
			CreatedAt:  r.CreatedAt.Time,
		})
	}
	return out, nil
}

// ------------------------------------------------------------
// internal
// ------------------------------------------------------------

func (h *hubDB) nextSeq(ctx context.Context, sessionID string) (int, error) {
	type row struct {
		Seq int `json:"seq"`
	}
	rows, err := runQuery[[]row](ctx, h.querier,
		`query ($sid: String!) {
			hub { db { agent {
				session_events(
					filter: {session_id: {eq: $sid}},
					order_by: [{field: "seq", direction: DESC}],
					limit: 1
				) { seq }
			}}}
		}`,
		map[string]any{"sid": sessionID},
		"hub.db.agent.session_events",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return 1, nil
		}
		return 0, err
	}
	if len(rows) == 0 {
		return 1, nil
	}
	return rows[0].Seq + 1, nil
}

func defaultString(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}
