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
