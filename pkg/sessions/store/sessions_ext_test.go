//go:build duckdb_arrow

package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
)

// TestSessions_GetSession covers the single-session lookup path that
// Manager.RestoreOpen uses.
func TestSessions_GetSession(t *testing.T) {
	h := newClient(t, "agt_ag01", "ag01")
	ctx := context.Background()

	_, err := h.CreateSession(ctx, sessstore.Record{
		ID: "sess-get", OwnerID: "u1", Status: "active", Mission: "hello",
	})
	require.NoError(t, err)

	got, err := h.GetSession(ctx, "sess-get")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "sess-get", got.ID)
	assert.Equal(t, "u1", got.OwnerID)
	assert.Equal(t, "hello", got.Mission)

	// Missing session → (nil, nil).
	miss, err := h.GetSession(ctx, "ghost")
	require.NoError(t, err)
	assert.Nil(t, miss)
}

// TestSessions_ListChildSessions exercises parent_session_id +
// fork_after_seq population for sub-agent sessions.
func TestSessions_ListChildSessions(t *testing.T) {
	h := newClient(t, "agt_ag01", "ag01")
	ctx := context.Background()

	_, err := h.CreateSession(ctx, sessstore.Record{
		ID: "sess-parent", OwnerID: "u1", Status: "active",
	})
	require.NoError(t, err)

	seq1 := 3
	_, err = h.CreateSession(ctx, sessstore.Record{
		ID: "sess-child-1", OwnerID: "u1", Status: "active",
		ParentSessionID: "sess-parent", ForkAfterSeq: &seq1,
	})
	require.NoError(t, err)

	seq2 := 5
	_, err = h.CreateSession(ctx, sessstore.Record{
		ID: "sess-child-2", OwnerID: "u1", Status: "active",
		ParentSessionID: "sess-parent", ForkAfterSeq: &seq2,
	})
	require.NoError(t, err)

	children, err := h.ListChildSessions(ctx, "sess-parent")
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, c := range children {
		ids[c.ID] = true
	}
	assert.True(t, ids["sess-child-1"])
	assert.True(t, ids["sess-child-2"])

	// Session with no children returns empty list.
	orphans, err := h.ListChildSessions(ctx, "sess-child-1")
	require.NoError(t, err)
	assert.Empty(t, orphans)
}

// TestSessions_GetEventsFullAndCountToolCalls verifies the reviewer-
// facing view: full event rows + a scalar count of tool_call events.
func TestSessions_GetEventsFullAndCountToolCalls(t *testing.T) {
	h := newClient(t, "agt_ag01", "ag01")
	ctx := context.Background()

	_, err := h.CreateSession(ctx, sessstore.Record{
		ID: "sess-full", OwnerID: "u1", Status: "active",
	})
	require.NoError(t, err)

	// Mix of event types: 2 tool_call, 1 tool_result, 1 user_message.
	for _, ev := range []sessstore.Event{
		{SessionID: "sess-full", EventType: sessstore.EventTypeToolCall,
			Author: "sess-full", Content: "discovery-catalog",
			Metadata: map[string]any{"args": "{}"}},
		{SessionID: "sess-full", EventType: sessstore.EventTypeToolResult,
			Author: "sess-full", Content: "{ok}"},
		{SessionID: "sess-full", EventType: sessstore.EventTypeToolCall,
			Author: "sess-full", Content: "data-query",
			Metadata: map[string]any{"args": "{limit:10}"}},
		{SessionID: "sess-full", EventType: sessstore.EventTypeUserMessage,
			Author: "u1", Content: "hi"},
	} {
		_, err := h.AppendEvent(ctx, ev)
		require.NoError(t, err)
	}

	n, err := h.CountToolCalls(ctx, "sess-full")
	require.NoError(t, err)
	assert.Equal(t, 2, n, "two tool_call events expected")

	full, err := h.GetEventsFull(ctx, "sess-full")
	require.NoError(t, err)
	require.Len(t, full, 4)
	// Seq is assigned incrementally per session (1..4).
	for i, ev := range full {
		assert.Equal(t, i+1, ev.Seq, "event %d seq mismatch", i)
	}
	// Metadata survives round-trip for tool_call events.
	var sawCatalog bool
	for _, ev := range full {
		if ev.EventType == sessstore.EventTypeToolCall && ev.Content == "discovery-catalog" {
			sawCatalog = true
			assert.NotEmpty(t, ev.Metadata)
		}
	}
	assert.True(t, sawCatalog, "discovery-catalog tool_call with metadata not found")
}

// TestSessions_CountToolCalls_NoEvents ensures an empty session
// returns 0 (not an error).
func TestSessions_CountToolCalls_NoEvents(t *testing.T) {
	h := newClient(t, "agt_ag01", "ag01")
	ctx := context.Background()
	_, err := h.CreateSession(ctx, sessstore.Record{
		ID: "sess-empty", OwnerID: "u1", Status: "active",
	})
	require.NoError(t, err)

	n, err := h.CountToolCalls(ctx, "sess-empty")
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

// ----- spec 006 — session_type / spawned_from_event_id / notes chain -----

// TestSessions_SubAgentLinkage covers Record's new spec-006 fields
// (SessionType + SpawnedFromEventID): they're round-tripped through
// CreateSession + GetSession unchanged.
func TestSessions_SubAgentLinkage(t *testing.T) {
	h := newClient(t, "agt_ag01", "ag01")
	ctx := context.Background()

	// Coordinator (root) session.
	_, err := h.CreateSession(ctx, sessstore.Record{
		ID: "sess-coord", OwnerID: "u1", Status: "active",
		SessionType: sessstore.SessionTypeRoot,
	})
	require.NoError(t, err)

	// Append a tool_call event on the coord (the dispatch event id we'll
	// link from the child).
	dispatchEvent := sessstore.Event{
		SessionID: "sess-coord", EventType: sessstore.EventTypeToolCall,
		Author: "agent", Content: "subagent_hugr-data_schema_explorer",
		ToolName: "subagent_hugr-data_schema_explorer",
	}
	dispatchEventID, err := h.AppendEvent(ctx, dispatchEvent)
	require.NoError(t, err)

	// Sub-agent (specialist) session.
	_, err = h.CreateSession(ctx, sessstore.Record{
		ID:                 "sess-spec",
		OwnerID:            "u1",
		Status:             "active",
		ParentSessionID:    "sess-coord",
		SessionType:        sessstore.SessionTypeSubAgent,
		SpawnedFromEventID: dispatchEventID,
		Mission:            "describe tf.incidents",
	})
	require.NoError(t, err)

	got, err := h.GetSession(ctx, "sess-spec")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, sessstore.SessionTypeSubAgent, got.SessionType)
	assert.Equal(t, "sess-coord", got.ParentSessionID)
	assert.Equal(t, dispatchEventID, got.SpawnedFromEventID)
	assert.Equal(t, "describe tf.incidents", got.Mission)

	// Coordinator round-trip preserves session_type = "root".
	gotCoord, err := h.GetSession(ctx, "sess-coord")
	require.NoError(t, err)
	require.NotNil(t, gotCoord)
	assert.Equal(t, sessstore.SessionTypeRoot, gotCoord.SessionType)
	assert.Empty(t, gotCoord.SpawnedFromEventID)
	assert.Empty(t, gotCoord.ParentSessionID)
}

// TestSessions_NotesChain exercises ListNotesChain — sub-agent writes
// a note targeted at the parent (scope=parent), root writes its own
// note; the chain query from the sub-agent's vantage returns BOTH
// own and parent's, ordered own-first.
func TestSessions_NotesChain(t *testing.T) {
	h := newClient(t, "agt_ag01", "ag01")
	ctx := context.Background()

	// Coordinator + sub-agent sessions.
	_, err := h.CreateSession(ctx, sessstore.Record{
		ID: "sess-coord-2", OwnerID: "u1", Status: "active",
		SessionType: sessstore.SessionTypeRoot,
	})
	require.NoError(t, err)
	_, err = h.CreateSession(ctx, sessstore.Record{
		ID: "sess-sub-2", OwnerID: "u1", Status: "active",
		ParentSessionID: "sess-coord-2",
		SessionType:     sessstore.SessionTypeSubAgent,
	})
	require.NoError(t, err)

	// 1. Root writes its own note (scope=self → session_id == author == coord).
	_, err = h.AddNote(ctx, sessstore.Note{
		SessionID: "sess-coord-2",
		Content:   "user wants BW Q1",
	})
	require.NoError(t, err)

	// 2. Sub-agent writes a note "up" to the parent
	//    (scope=parent → session_id = coord, author = sub).
	_, err = h.AddNote(ctx, sessstore.Note{
		SessionID:       "sess-coord-2",
		AuthorSessionID: "sess-sub-2",
		Content:         "station_id FK -> stations",
	})
	require.NoError(t, err)

	// 3. Sub-agent writes a self-scoped note for its own working set.
	_, err = h.AddNote(ctx, sessstore.Note{
		SessionID: "sess-sub-2",
		Content:   "20 fields discovered",
	})
	require.NoError(t, err)

	// Chain query from the sub-agent's vantage: own (depth 0) +
	// parent's (depth 1).
	chain, err := h.ListNotesChain(ctx, "sess-sub-2")
	require.NoError(t, err)
	require.Len(t, chain, 3)

	// Depth 0 first (own note).
	assert.Equal(t, 0, chain[0].ChainDepth)
	assert.Equal(t, "sess-sub-2", chain[0].SessionID)
	assert.Equal(t, "20 fields discovered", chain[0].Content)

	// Depth 1: two parent notes (rendered most-recent first).
	assert.Equal(t, 1, chain[1].ChainDepth)
	assert.Equal(t, 1, chain[2].ChainDepth)

	// Authorship is preserved across the chain — the promoted note keeps
	// AuthorSessionID = "sess-sub-2" even though SessionID = "sess-coord-2".
	var foundPromoted bool
	for _, n := range chain {
		if n.Content == "station_id FK -> stations" {
			foundPromoted = true
			assert.Equal(t, "sess-coord-2", n.SessionID)
			assert.Equal(t, "sess-sub-2", n.AuthorSessionID)
		}
	}
	assert.True(t, foundPromoted, "promoted note from sub-agent should appear in chain")

	// Chain query from the coordinator's vantage: only its own scope
	// (sub-agent's self-note depth=1 NOT visible — it lives "below").
	coordChain, err := h.ListNotesChain(ctx, "sess-coord-2")
	require.NoError(t, err)
	require.Len(t, coordChain, 2)
	for _, n := range coordChain {
		assert.Equal(t, 0, n.ChainDepth)
		assert.Equal(t, "sess-coord-2", n.SessionID)
	}
}
