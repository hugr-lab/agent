package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/pkg/store/queries"

	"github.com/hugr-lab/hugen/pkg/id"
)

// CreateSession inserts a new row into hub.db.agent.sessions. Idempotent:
// if a row with the same ID already exists, returns that ID.
func (c *Client) CreateSession(ctx context.Context, s Record) (string, error) {
	if s.ID == "" {
		return "", fmt.Errorf("hubdb: CreateSession requires ID")
	}
	if s.AgentID == "" {
		s.AgentID = c.agentID
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
	err := queries.RunMutation(ctx, c.querier,
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
func (c *Client) UpdateSessionStatus(ctx context.Context, id, status string) error {
	return queries.RunMutation(ctx, c.querier,
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
func (c *Client) ListActiveSessions(ctx context.Context) ([]Record, error) {
	type row struct {
		ID              string         `json:"id"`
		AgentID         string         `json:"agent_id"`
		OwnerID         string         `json:"owner_id"`
		ParentSessionID string         `json:"parent_session_id"`
		ForkAfterSeq    *int           `json:"fork_after_seq"`
		Status          string         `json:"status"`
		Mission         string         `json:"mission"`
		Metadata        map[string]any `json:"metadata"`
		CreatedAt       time.Time         `json:"created_at"`
		UpdatedAt       time.Time         `json:"updated_at"`
	}
	rows, err := queries.RunQuery[[]row](ctx, c.querier,
		`query ($agent: String!) {
			hub { db { agent {
				sessions(filter: {agent_id: {eq: $agent}, status: {eq: "active"}}) {
					id agent_id owner_id parent_session_id fork_after_seq status mission metadata created_at updated_at
				}
			}}}
		}`,
		map[string]any{"agent": c.agentID},
		"hub.db.agent.sessions",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Record, 0, len(rows))
	for _, r := range rows {
		out = append(out, Record{
			ID:              r.ID,
			AgentID:         r.AgentID,
			OwnerID:         r.OwnerID,
			ParentSessionID: r.ParentSessionID,
			ForkAfterSeq:    r.ForkAfterSeq,
			Status:          r.Status,
			Mission:         r.Mission,
			Metadata:        r.Metadata,
			CreatedAt:       r.CreatedAt,
			UpdatedAt:       r.UpdatedAt,
		})
	}
	return out, nil
}

// AppendEvent inserts a row into hub.db.agent.session_events. Computes seq
// as max(seq)+1 within the session (not transactionally atomic; fine for
// single-writer-per-session which is how ADK drives sessions).
func (c *Client) AppendEvent(ctx context.Context, ev Event) (string, error) {
	if ev.SessionID == "" {
		return "", fmt.Errorf("hubdb: AppendEvent requires SessionID")
	}
	if ev.EventType == "" {
		return "", fmt.Errorf("hubdb: AppendEvent requires EventType")
	}
	if ev.AgentID == "" {
		ev.AgentID = c.agentID
	}
	seq := ev.Seq
	if seq == 0 {
		next, err := c.nextSeq(ctx, ev.SessionID)
		if err != nil {
			return "", err
		}
		seq = next
	}
	eventID := ev.ID
	if eventID == "" {
		eventID = id.New(id.PrefixEvent, c.agentShort)
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
	if err := queries.RunMutation(ctx, c.querier,
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
func (c *Client) GetEvents(ctx context.Context, sessionID string) ([]Event, error) {
	type row struct {
		ID         string          `json:"id"`
		SessionID  string          `json:"session_id"`
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
	rows, err := queries.RunQuery[[]row](ctx, c.querier,
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
	out := make([]Event, 0, len(rows))
	for _, r := range rows {
		out = append(out, Event{
			ID:         r.ID,
			SessionID:  r.SessionID,
			AgentID:    r.AgentID,
			Seq:        r.Seq,
			EventType:  r.EventType,
			Author:     r.Author,
			Content:    r.Content,
			ToolName:   r.ToolName,
			ToolArgs:   r.ToolArgs,
			ToolResult: r.ToolResult,
			Metadata:   r.Metadata,
			CreatedAt:  r.CreatedAt,
		})
	}
	return out, nil
}

// GetSession fetches a single session row. Returns (nil, nil) when
// the session is not found.
func (c *Client) GetSession(ctx context.Context, id string) (*Record, error) {
	type row struct {
		ID              string         `json:"id"`
		AgentID         string         `json:"agent_id"`
		OwnerID         string         `json:"owner_id"`
		ParentSessionID string         `json:"parent_session_id"`
		ForkAfterSeq    *int           `json:"fork_after_seq"`
		Status          string         `json:"status"`
		Mission         string         `json:"mission"`
		Metadata        map[string]any `json:"metadata"`
		CreatedAt       time.Time         `json:"created_at"`
		UpdatedAt       time.Time         `json:"updated_at"`
	}
	rows, err := queries.RunQuery[[]row](ctx, c.querier,
		`query ($agent: String!, $id: String!) {
			hub { db { agent {
				sessions(filter: {agent_id: {eq: $agent}, id: {eq: $id}}, limit: 1) {
					id agent_id owner_id parent_session_id fork_after_seq status mission metadata created_at updated_at
				}
			}}}
		}`,
		map[string]any{"agent": c.agentID, "id": id},
		"hub.db.agent.sessions",
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
	return &Record{
		ID:              r.ID,
		AgentID:         r.AgentID,
		OwnerID:         r.OwnerID,
		ParentSessionID: r.ParentSessionID,
		ForkAfterSeq:    r.ForkAfterSeq,
		Status:          r.Status,
		Mission:         r.Mission,
		Metadata:        r.Metadata,
		CreatedAt:       r.CreatedAt,
		UpdatedAt:       r.UpdatedAt,
	}, nil
}

// ListChildSessions returns every session whose parent_session_id
// matches parentSessionID. Empty slice when none exist.
func (c *Client) ListChildSessions(ctx context.Context, parentSessionID string) ([]Record, error) {
	type row struct {
		ID              string         `json:"id"`
		AgentID         string         `json:"agent_id"`
		OwnerID         string         `json:"owner_id"`
		ParentSessionID string         `json:"parent_session_id"`
		ForkAfterSeq    *int           `json:"fork_after_seq"`
		Status          string         `json:"status"`
		Mission         string         `json:"mission"`
		Metadata        map[string]any `json:"metadata"`
		CreatedAt       time.Time         `json:"created_at"`
		UpdatedAt       time.Time         `json:"updated_at"`
	}
	rows, err := queries.RunQuery[[]row](ctx, c.querier,
		`query ($agent: String!, $parent: String!) {
			hub { db { agent {
				sessions(filter: {agent_id: {eq: $agent}, parent_session_id: {eq: $parent}}) {
					id agent_id owner_id parent_session_id fork_after_seq status mission metadata created_at updated_at
				}
			}}}
		}`,
		map[string]any{"agent": c.agentID, "parent": parentSessionID},
		"hub.db.agent.sessions",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Record, 0, len(rows))
	for _, r := range rows {
		out = append(out, Record{
			ID:              r.ID,
			AgentID:         r.AgentID,
			OwnerID:         r.OwnerID,
			ParentSessionID: r.ParentSessionID,
			ForkAfterSeq:    r.ForkAfterSeq,
			Status:          r.Status,
			Mission:         r.Mission,
			Metadata:        r.Metadata,
			CreatedAt:       r.CreatedAt,
			UpdatedAt:       r.UpdatedAt,
		})
	}
	return out, nil
}

// GetEventsFull returns the full event history for a session through
// the session_events_full view, which recursively includes inherited
// events from the parent chain for forked sessions. Sub-agents (no
// fork_after_seq) see only their own events.
func (c *Client) GetEventsFull(ctx context.Context, sessionID string) ([]EventFull, error) {
	type row struct {
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
		ChainDepth int            `json:"chain_depth"`
	}
	rows, err := queries.RunQuery[[]row](ctx, c.querier,
		`query ($input: session_events_full_input!) {
			hub { db { agent {
				session_events_full(session_events_full_input: $input) {
					id session_id agent_id seq event_type author content
					tool_name tool_args tool_result metadata created_at chain_depth
				}
			}}}
		}`,
		map[string]any{"input": map[string]any{"session_id": sessionID}},
		"hub.db.agent.session_events_full",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]EventFull, 0, len(rows))
	for _, r := range rows {
		out = append(out, EventFull{
			Event: Event{
				ID:         r.ID,
				SessionID:  r.SessionID,
				AgentID:    r.AgentID,
				Seq:        r.Seq,
				EventType:  r.EventType,
				Author:     r.Author,
				Content:    r.Content,
				ToolName:   r.ToolName,
				ToolArgs:   r.ToolArgs,
				ToolResult: r.ToolResult,
				Metadata:   r.Metadata,
				CreatedAt:  r.CreatedAt,
			},
			ChainDepth: r.ChainDepth,
		})
	}
	return out, nil
}

// CountToolCalls counts the tool_call rows in a session's transcript.
func (c *Client) CountToolCalls(ctx context.Context, sessionID string) (int, error) {
	type row struct {
		Seq int `json:"seq"`
	}
	// query-engine may not expose aggregate(count) across all deployments;
	// fetch IDs with limit and count client-side. Cheap at session scale
	// (<1k rows per session).
	rows, err := queries.RunQuery[[]row](ctx, c.querier,
		`query ($sid: String!) {
			hub { db { agent {
				session_events(filter: {session_id: {eq: $sid}, event_type: {eq: "tool_call"}}) { seq }
			}}}
		}`,
		map[string]any{"sid": sessionID},
		"hub.db.agent.session_events",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return 0, nil
		}
		return 0, err
	}
	return len(rows), nil
}

// ------------------------------------------------------------
// Session notes
// ------------------------------------------------------------

// AddNote inserts a session_notes row and returns the assigned ID.
// Caller may supply note.ID; otherwise pkg/id.New(PrefixNote, short)
// generates one. Session notes are the LLM's scratchpad — they live
// in the system prompt's fixed part and survive context compaction.
func (c *Client) AddNote(ctx context.Context, note Note) (string, error) {
	if note.SessionID == "" {
		return "", fmt.Errorf("hubdb: AddNote requires SessionID")
	}
	if note.Content == "" {
		return "", fmt.Errorf("hubdb: AddNote requires Content")
	}
	if note.ID == "" {
		note.ID = id.New(id.PrefixNote, c.agentShort)
	}
	if note.AgentID == "" {
		note.AgentID = c.agentID
	}
	data := map[string]any{
		"id":         note.ID,
		"agent_id":   note.AgentID,
		"session_id": note.SessionID,
		"content":    note.Content,
	}
	if err := queries.RunMutation(ctx, c.querier,
		`mutation ($data: hub_db_session_notes_mut_input_data!) {
			hub { db { agent {
				insert_session_notes(data: $data) { id }
			}}}
		}`,
		map[string]any{"data": data},
	); err != nil {
		return "", err
	}
	return note.ID, nil
}

// ListNotes returns every note in a session ordered by created_at ASC.
func (c *Client) ListNotes(ctx context.Context, sessionID string) ([]Note, error) {
	type row struct {
		ID        string `json:"id"`
		AgentID   string `json:"agent_id"`
		SessionID string `json:"session_id"`
		Content   string `json:"content"`
		CreatedAt time.Time `json:"created_at"`
	}
	rows, err := queries.RunQuery[[]row](ctx, c.querier,
		`query ($sid: String!) {
			hub { db { agent {
				session_notes(filter: {session_id: {eq: $sid}}, order_by: [{field: "created_at", direction: ASC}]) {
					id agent_id session_id content created_at
				}
			}}}
		}`,
		map[string]any{"sid": sessionID},
		"hub.db.agent.session_notes",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Note, 0, len(rows))
	for _, r := range rows {
		out = append(out, Note{
			ID:        r.ID,
			AgentID:   r.AgentID,
			SessionID: r.SessionID,
			Content:   r.Content,
			CreatedAt: r.CreatedAt,
		})
	}
	return out, nil
}

// DeleteNote removes a single note by ID.
func (c *Client) DeleteNote(ctx context.Context, noteID string) error {
	return queries.RunMutation(ctx, c.querier,
		`mutation ($id: String!) {
			hub { db { agent {
				delete_session_notes(filter: {id: {eq: $id}}) { affected_rows }
			}}}
		}`,
		map[string]any{"id": noteID},
	)
}

// DeleteSessionNotes removes every note in a session. Returns the
// affected_rows count reported by the mutation (best-effort; 0 on
// parse failure is not treated as an error).
func (c *Client) DeleteSessionNotes(ctx context.Context, sessionID string) (int, error) {
	type result struct {
		Affected int `json:"affected_rows"`
	}
	resp, err := c.querier.Query(ctx,
		`mutation ($sid: String!) {
			hub { db { agent {
				delete_session_notes(filter: {session_id: {eq: $sid}}) { affected_rows }
			}}}
		}`,
		map[string]any{"sid": sessionID},
	)
	if err != nil {
		return 0, fmt.Errorf("hubdb mutation: %w", err)
	}
	defer resp.Close()
	if err := resp.Err(); err != nil {
		return 0, fmt.Errorf("hubdb graphql: %w", err)
	}
	var r result
	if err := resp.ScanData("hub.db.agent.delete_session_notes", &r); err != nil {
		if !errors.Is(err, types.ErrWrongDataPath) && !errors.Is(err, types.ErrNoData) {
			return 0, fmt.Errorf("hubdb scan: %w", err)
		}
	}
	return r.Affected, nil
}

// ------------------------------------------------------------
// Session participants
// ------------------------------------------------------------

// AddParticipant inserts a session_participants row.
func (c *Client) AddParticipant(ctx context.Context, p Participant) error {
	if p.SessionID == "" || p.UserID == "" {
		return fmt.Errorf("hubdb: AddParticipant requires SessionID + UserID")
	}
	role := p.Role
	if role == "" {
		role = "participant"
	}
	data := map[string]any{
		"session_id": p.SessionID,
		"user_id":    p.UserID,
		"role":       role,
	}
	return queries.RunMutation(ctx, c.querier,
		`mutation ($data: hub_db_session_participants_mut_input_data!) {
			hub { db { agent {
				insert_session_participants(data: $data) { session_id user_id }
			}}}
		}`,
		map[string]any{"data": data},
	)
}

// RemoveParticipant hard-deletes a participant row. An audit-preserving
// variant (set left_at=now) can land later if a need arises.
func (c *Client) RemoveParticipant(ctx context.Context, sessionID, userID string) error {
	return queries.RunMutation(ctx, c.querier,
		`mutation ($sid: String!, $uid: String!) {
			hub { db { agent {
				delete_session_participants(filter: {session_id: {eq: $sid}, user_id: {eq: $uid}}) {
					affected_rows
				}
			}}}
		}`,
		map[string]any{"sid": sessionID, "uid": userID},
	)
}

// ListParticipants returns every participant row for a session.
func (c *Client) ListParticipants(ctx context.Context, sessionID string) ([]Participant, error) {
	type row struct {
		SessionID string  `json:"session_id"`
		UserID    string  `json:"user_id"`
		Role      string  `json:"role"`
		JoinedAt  time.Time  `json:"joined_at"`
		LeftAt    *time.Time `json:"left_at"`
	}
	rows, err := queries.RunQuery[[]row](ctx, c.querier,
		`query ($sid: String!) {
			hub { db { agent {
				session_participants(filter: {session_id: {eq: $sid}}) {
					session_id user_id role joined_at left_at
				}
			}}}
		}`,
		map[string]any{"sid": sessionID},
		"hub.db.agent.session_participants",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Participant, 0, len(rows))
	for _, r := range rows {
		p := Participant{
			SessionID: r.SessionID,
			UserID:    r.UserID,
			Role:      r.Role,
			JoinedAt:  r.JoinedAt,
		}
		if r.LeftAt != nil && !r.LeftAt.IsZero() {
			p.LeftAt = r.LeftAt
		}
		out = append(out, p)
	}
	return out, nil
}

// ------------------------------------------------------------
// internal
// ------------------------------------------------------------

func (c *Client) nextSeq(ctx context.Context, sessionID string) (int, error) {
	type row struct {
		Seq int `json:"seq"`
	}
	rows, err := queries.RunQuery[[]row](ctx, c.querier,
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
