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
	sessionType := s.SessionType
	if sessionType == "" {
		// Schema column is NOT NULL with default 'root'; explicit value here
		// keeps the Hugr GraphQL mutation input check happy regardless of how
		// the engine treats column defaults.
		sessionType = SessionTypeRoot
	}
	data := map[string]any{
		"id":           s.ID,
		"agent_id":     s.AgentID,
		"status":       status,
		"session_type": sessionType,
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
	if s.SpawnedFromEventID != "" {
		data["spawned_from_event_id"] = s.SpawnedFromEventID
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
		ID                 string         `json:"id"`
		AgentID            string         `json:"agent_id"`
		OwnerID            string         `json:"owner_id"`
		ParentSessionID    string         `json:"parent_session_id"`
		ForkAfterSeq       *int           `json:"fork_after_seq"`
		SessionType        string         `json:"session_type"`
		SpawnedFromEventID string         `json:"spawned_from_event_id"`
		Status             string         `json:"status"`
		Mission            string         `json:"mission"`
		Metadata           map[string]any `json:"metadata"`
		CreatedAt          time.Time      `json:"created_at"`
		UpdatedAt          time.Time      `json:"updated_at"`
	}
	rows, err := queries.RunQuery[[]row](ctx, c.querier,
		`query ($agent: String!) {
			hub { db { agent {
				sessions(filter: {agent_id: {eq: $agent}, status: {eq: "active"}}) {
					id agent_id owner_id parent_session_id fork_after_seq session_type spawned_from_event_id status mission metadata created_at updated_at
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
			ID:                 r.ID,
			AgentID:            r.AgentID,
			OwnerID:            r.OwnerID,
			ParentSessionID:    r.ParentSessionID,
			ForkAfterSeq:       r.ForkAfterSeq,
			SessionType:        r.SessionType,
			SpawnedFromEventID: r.SpawnedFromEventID,
			Status:             r.Status,
			Mission:            r.Mission,
			Metadata:           r.Metadata,
			CreatedAt:          r.CreatedAt,
			UpdatedAt:          r.UpdatedAt,
		})
	}
	return out, nil
}

// AppendEvent inserts a row into hub.db.agent.session_events. Computes seq
// as max(seq)+1 within the session (not transactionally atomic; fine for
// single-writer-per-session which is how ADK drives sessions).
//
// Thin wrapper around AppendEventWithSummary with an empty summary —
// use AppendEventWithSummary directly when you want Hugr to compute +
// store the row's embedding as part of the insert.
func (c *Client) AppendEvent(ctx context.Context, ev Event) (string, error) {
	return c.AppendEventWithSummary(ctx, ev, "")
}

// AppendEventWithSummary inserts a row and, when summary is non-empty,
// asks Hugr to embed the summary text through the attached embedder
// and write the resulting vector to the `embedding` column atomically
// with the row (spec 006 §2). Empty summary → no `summary:` GraphQL
// argument and the row lands with embedding NULL (same behaviour as
// plain AppendEvent).
//
// Hugr reports embedder-side failures as mutation errors. The classifier
// catches those and retries once with summary="" (plain insert) — the
// row still persists so the agent's transcript stays complete even when
// the embedder is unavailable.
func (c *Client) AppendEventWithSummary(ctx context.Context, ev Event, summary string) (string, error) {
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
	// Gate the `summary:` argument behind the embedder flag: when the
	// engine has no embedder attached the schema doesn't expose the
	// argument and the server rejects the mutation outright ("Unknown
	// argument summary"). Tests that spin a minimal hugr engine hit
	// that path.
	if summary == "" || !c.embedderEnabled {
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
	if err := queries.RunMutation(ctx, c.querier,
		`mutation ($data: hub_db_session_events_mut_input_data!, $summary: String) {
			hub { db { agent {
				insert_session_events(data: $data, summary: $summary) { id }
			}}}
		}`,
		map[string]any{"data": data, "summary": summary},
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
		ID                 string         `json:"id"`
		AgentID            string         `json:"agent_id"`
		OwnerID            string         `json:"owner_id"`
		ParentSessionID    string         `json:"parent_session_id"`
		ForkAfterSeq       *int           `json:"fork_after_seq"`
		SessionType        string         `json:"session_type"`
		SpawnedFromEventID string         `json:"spawned_from_event_id"`
		Status             string         `json:"status"`
		Mission            string         `json:"mission"`
		Metadata           map[string]any `json:"metadata"`
		CreatedAt          time.Time      `json:"created_at"`
		UpdatedAt          time.Time      `json:"updated_at"`
	}
	rows, err := queries.RunQuery[[]row](ctx, c.querier,
		`query ($agent: String!, $id: String!) {
			hub { db { agent {
				sessions(filter: {agent_id: {eq: $agent}, id: {eq: $id}}, limit: 1) {
					id agent_id owner_id parent_session_id fork_after_seq session_type spawned_from_event_id status mission metadata created_at updated_at
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
		ID:                 r.ID,
		AgentID:            r.AgentID,
		OwnerID:            r.OwnerID,
		ParentSessionID:    r.ParentSessionID,
		ForkAfterSeq:       r.ForkAfterSeq,
		SessionType:        r.SessionType,
		SpawnedFromEventID: r.SpawnedFromEventID,
		Status:             r.Status,
		Mission:            r.Mission,
		Metadata:           r.Metadata,
		CreatedAt:          r.CreatedAt,
		UpdatedAt:          r.UpdatedAt,
	}, nil
}

// ListChildSessions returns every session whose parent_session_id
// matches parentSessionID. Empty slice when none exist.
func (c *Client) ListChildSessions(ctx context.Context, parentSessionID string) ([]Record, error) {
	type row struct {
		ID                 string         `json:"id"`
		AgentID            string         `json:"agent_id"`
		OwnerID            string         `json:"owner_id"`
		ParentSessionID    string         `json:"parent_session_id"`
		ForkAfterSeq       *int           `json:"fork_after_seq"`
		SessionType        string         `json:"session_type"`
		SpawnedFromEventID string         `json:"spawned_from_event_id"`
		Status             string         `json:"status"`
		Mission            string         `json:"mission"`
		Metadata           map[string]any `json:"metadata"`
		CreatedAt          time.Time      `json:"created_at"`
		UpdatedAt          time.Time      `json:"updated_at"`
	}
	rows, err := queries.RunQuery[[]row](ctx, c.querier,
		`query ($agent: String!, $parent: String!) {
			hub { db { agent {
				sessions(filter: {agent_id: {eq: $agent}, parent_session_id: {eq: $parent}}) {
					id agent_id owner_id parent_session_id fork_after_seq session_type spawned_from_event_id status mission metadata created_at updated_at
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
			ID:                 r.ID,
			AgentID:            r.AgentID,
			OwnerID:            r.OwnerID,
			ParentSessionID:    r.ParentSessionID,
			ForkAfterSeq:       r.ForkAfterSeq,
			SessionType:        r.SessionType,
			SpawnedFromEventID: r.SpawnedFromEventID,
			Status:             r.Status,
			Mission:            r.Mission,
			Metadata:           r.Metadata,
			CreatedAt:          r.CreatedAt,
			UpdatedAt:          r.UpdatedAt,
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
		`query ($input: hub_db_session_events_full_input!) {
			hub { db { agent {
				session_events_full(args: $input) {
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
	// Default author to viewer (self-scope write) so existing callers that
	// don't set AuthorSessionID continue to behave as before. memory_note's
	// "parent" / "ancestors" scopes set this explicitly to the writing
	// session so the note's authorship survives a cross-scope promotion.
	if note.AuthorSessionID == "" {
		note.AuthorSessionID = note.SessionID
	}
	data := map[string]any{
		"id":                note.ID,
		"agent_id":          note.AgentID,
		"session_id":        note.SessionID,
		"author_session_id": note.AuthorSessionID,
		"content":           note.Content,
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
//
// Returns notes whose session_id matches sessionID — i.e. only notes
// that are visible at this session's level. Cross-session notes
// promoted up the chain via memory_note(scope: "parent" | "ancestors")
// land here when sessionID is the chain target. To get the full chain
// (own + ancestor notes addressed up here from below), use the
// session_notes_chain view via ListNotesChain.
func (c *Client) ListNotes(ctx context.Context, sessionID string) ([]Note, error) {
	type row struct {
		ID              string    `json:"id"`
		AgentID         string    `json:"agent_id"`
		SessionID       string    `json:"session_id"`
		AuthorSessionID string    `json:"author_session_id"`
		Content         string    `json:"content"`
		CreatedAt       time.Time `json:"created_at"`
	}
	rows, err := queries.RunQuery[[]row](ctx, c.querier,
		`query ($sid: String!) {
			hub { db { agent {
				session_notes(filter: {session_id: {eq: $sid}}, order_by: [{field: "created_at", direction: ASC}]) {
					id agent_id session_id author_session_id content created_at
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
			ID:              r.ID,
			AgentID:         r.AgentID,
			SessionID:       r.SessionID,
			AuthorSessionID: r.AuthorSessionID,
			Content:         r.Content,
			CreatedAt:       r.CreatedAt,
		})
	}
	return out, nil
}

// NoteWithDepth carries chain depth alongside a Note for callers that
// query the session_notes_chain view (spec 006). chain_depth = 0
// means "own note"; > 0 means "ancestor note visible because this
// session is its scope target".
type NoteWithDepth struct {
	Note
	ChainDepth int `json:"chain_depth"`
}

// ListNotesChain returns every note visible to the given session by
// querying the session_notes_chain view (recursive walk over
// parent_session_id, depth cap 8). Used by Session.Snapshot's
// "## Session notes" block (spec 006). Ordering is the view's
// (chain_depth ASC, created_at DESC) — own notes first.
func (c *Client) ListNotesChain(ctx context.Context, sessionID string) ([]NoteWithDepth, error) {
	type row struct {
		ID              string    `json:"id"`
		AgentID         string    `json:"agent_id"`
		SessionID       string    `json:"session_id"`
		AuthorSessionID string    `json:"author_session_id"`
		Content         string    `json:"content"`
		CreatedAt       time.Time `json:"created_at"`
		ChainDepth      int       `json:"chain_depth"`
	}
	rows, err := queries.RunQuery[[]row](ctx, c.querier,
		`query ($input: hub_db_session_notes_chain_input!) {
			hub { db { agent {
				session_notes_chain(args: $input) {
					id agent_id session_id author_session_id content created_at chain_depth
				}
			}}}
		}`,
		map[string]any{"input": map[string]any{"session_id": sessionID}},
		"hub.db.agent.session_notes_chain",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]NoteWithDepth, 0, len(rows))
	for _, r := range rows {
		out = append(out, NoteWithDepth{
			Note: Note{
				ID:              r.ID,
				AgentID:         r.AgentID,
				SessionID:       r.SessionID,
				AuthorSessionID: r.AuthorSessionID,
				Content:         r.Content,
				CreatedAt:       r.CreatedAt,
			},
			ChainDepth: r.ChainDepth,
		})
	}
	return out, nil
}

// DeleteNote removes a single note by ID. Unconditional — callers at
// the store layer (reviewer, housekeeper, dev tooling) always have
// full authority. The author-only policy for the LLM-facing
// memory_clear_note tool lives above this layer; see DeleteNoteAsAuthor
// for the gated variant.
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

// DeleteNoteAsAuthor removes a note only when its author_session_id
// matches the caller's sessionID. Returns affected rows (0 when the
// author check fails — caller should treat that as a permission denial).
// The LLM-facing memory_clear_note tool gates deletes through this
// path so a specialist cannot wipe notes another session wrote, while
// housekeepers below the surface use DeleteNote for full authority.
func (c *Client) DeleteNoteAsAuthor(ctx context.Context, noteID, authorSessionID string) (int, error) {
	if noteID == "" {
		return 0, fmt.Errorf("hubdb: DeleteNoteAsAuthor requires noteID")
	}
	if authorSessionID == "" {
		return 0, fmt.Errorf("hubdb: DeleteNoteAsAuthor requires authorSessionID")
	}
	type result struct {
		Affected int `json:"affected_rows"`
	}
	resp, err := c.querier.Query(ctx,
		`mutation ($id: String!, $author: String!) {
			hub { db { agent {
				delete_session_notes(filter: {id: {eq: $id}, author_session_id: {eq: $author}}) {
					affected_rows
				}
			}}}
		}`,
		map[string]any{"id": noteID, "author": authorSessionID},
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

// GetNote fetches a single note by ID (any session). Returns
// (nil, nil) when the note does not exist.
func (c *Client) GetNote(ctx context.Context, noteID string) (*Note, error) {
	type row struct {
		ID              string    `json:"id"`
		AgentID         string    `json:"agent_id"`
		SessionID       string    `json:"session_id"`
		AuthorSessionID string    `json:"author_session_id"`
		Content         string    `json:"content"`
		CreatedAt       time.Time `json:"created_at"`
	}
	rows, err := queries.RunQuery[[]row](ctx, c.querier,
		`query ($id: String!) {
			hub { db { agent {
				session_notes(filter: {id: {eq: $id}}, limit: 1) {
					id agent_id session_id author_session_id content created_at
				}
			}}}
		}`,
		map[string]any{"id": noteID},
		"hub.db.agent.session_notes",
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
	return &Note{
		ID:              r.ID,
		AgentID:         r.AgentID,
		SessionID:       r.SessionID,
		AuthorSessionID: r.AuthorSessionID,
		Content:         r.Content,
		CreatedAt:       r.CreatedAt,
	}, nil
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
