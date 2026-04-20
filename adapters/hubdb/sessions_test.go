//go:build duckdb_arrow

package hubdb_test

import (
	"context"
	"testing"

	"github.com/hugr-lab/hugen/interfaces"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSessions_CRUD exercises the real GraphQL paths for CreateSession,
// UpdateSessionStatus, ListActiveSessions, AppendEvent, GetEvents.
// Runs against a fully-bootstrapped engine + seeded schema via the
// existing testEngine helper.
func TestSessions_CRUD(t *testing.T) {
	h := newTestHubDB(t, "agt_ag01", "ag01")
	ctx := context.Background()

	// Seed an agent instance so session rows FK-link correctly.
	err := h.RegisterAgent(ctx, interfaces.Agent{
		ID: "agt_ag01", AgentTypeID: "hugr-data", ShortID: "ag01",
		Name: "test-agent", Status: "active",
	})
	require.NoError(t, err)

	// Create two sessions, leave one closed, verify ListActive.
	_, err = h.CreateSession(ctx, interfaces.SessionRecord{
		ID: "sess-active", OwnerID: "u1", Status: "active", Mission: "active one",
	})
	require.NoError(t, err)
	_, err = h.CreateSession(ctx, interfaces.SessionRecord{
		ID: "sess-closed", OwnerID: "u1", Status: "active",
	})
	require.NoError(t, err)

	require.NoError(t, h.UpdateSessionStatus(ctx, "sess-closed", "closed"))

	active, err := h.ListActiveSessions(ctx)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, s := range active {
		ids[s.ID] = true
	}
	assert.True(t, ids["sess-active"], "active session missing from ListActiveSessions")
	assert.False(t, ids["sess-closed"], "closed session leaked into ListActiveSessions")

	// AppendEvent: skill_loaded, skill_unloaded. Seq auto-increments.
	_, err = h.AppendEvent(ctx, interfaces.SessionEvent{
		SessionID: "sess-active", EventType: interfaces.EventTypeSkillLoaded,
		Author: "sess-active", Content: "hugr-data",
		Metadata: map[string]any{"skill": "hugr-data"},
	})
	require.NoError(t, err)

	_, err = h.AppendEvent(ctx, interfaces.SessionEvent{
		SessionID: "sess-active", EventType: interfaces.EventTypeSkillUnloaded,
		Author: "sess-active", Content: "hugr-data",
		Metadata: map[string]any{"skill": "hugr-data"},
	})
	require.NoError(t, err)

	evs, err := h.GetEvents(ctx, "sess-active")
	require.NoError(t, err)
	require.Len(t, evs, 2)
	assert.Equal(t, interfaces.EventTypeSkillLoaded, evs[0].EventType)
	assert.Equal(t, interfaces.EventTypeSkillUnloaded, evs[1].EventType)
	assert.Equal(t, 1, evs[0].Seq)
	assert.Equal(t, 2, evs[1].Seq)
	assert.Equal(t, "hugr-data", evs[0].Content)
}

func TestSessions_Notes(t *testing.T) {
	h := newTestHubDB(t, "agt_ag01", "ag01")
	ctx := context.Background()
	require.NoError(t, h.RegisterAgent(ctx, interfaces.Agent{
		ID: "agt_ag01", AgentTypeID: "hugr-data", ShortID: "ag01", Name: "test",
	}))
	_, err := h.CreateSession(ctx, interfaces.SessionRecord{
		ID: "sess-notes", OwnerID: "u1", Status: "active",
	})
	require.NoError(t, err)

	id1, err := h.AddNote(ctx, interfaces.SessionNote{
		SessionID: "sess-notes", Content: "found 14 fields",
	})
	require.NoError(t, err)
	require.NotEmpty(t, id1)
	_, err = h.AddNote(ctx, interfaces.SessionNote{
		SessionID: "sess-notes", Content: "severity 1-3",
	})
	require.NoError(t, err)

	notes, err := h.ListNotes(ctx, "sess-notes")
	require.NoError(t, err)
	require.Len(t, notes, 2)
	assert.Equal(t, "found 14 fields", notes[0].Content)
	assert.Equal(t, "severity 1-3", notes[1].Content)

	require.NoError(t, h.DeleteNote(ctx, id1))
	notes, err = h.ListNotes(ctx, "sess-notes")
	require.NoError(t, err)
	require.Len(t, notes, 1)
	assert.Equal(t, "severity 1-3", notes[0].Content)

	n, err := h.DeleteSessionNotes(ctx, "sess-notes")
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, 0)
	notes, err = h.ListNotes(ctx, "sess-notes")
	require.NoError(t, err)
	assert.Empty(t, notes)
}

func TestSessions_Participants(t *testing.T) {
	h := newTestHubDB(t, "agt_ag01", "ag01")
	ctx := context.Background()
	require.NoError(t, h.RegisterAgent(ctx, interfaces.Agent{
		ID: "agt_ag01", AgentTypeID: "hugr-data", ShortID: "ag01", Name: "test",
	}))
	_, err := h.CreateSession(ctx, interfaces.SessionRecord{
		ID: "sess-p", OwnerID: "u1", Status: "active",
	})
	require.NoError(t, err)

	require.NoError(t, h.AddParticipant(ctx, interfaces.SessionParticipant{
		SessionID: "sess-p", UserID: "u1", Role: "owner",
	}))
	require.NoError(t, h.AddParticipant(ctx, interfaces.SessionParticipant{
		SessionID: "sess-p", UserID: "u2", Role: "observer",
	}))

	parts, err := h.ListParticipants(ctx, "sess-p")
	require.NoError(t, err)
	require.Len(t, parts, 2)

	require.NoError(t, h.RemoveParticipant(ctx, "sess-p", "u2"))
	parts, err = h.ListParticipants(ctx, "sess-p")
	require.NoError(t, err)
	require.Len(t, parts, 1)
	assert.Equal(t, "u1", parts[0].UserID)
}

func TestSessions_EmptyListActive(t *testing.T) {
	h := newTestHubDB(t, "agt_ag02", "ag02")
	ctx := context.Background()
	require.NoError(t, h.RegisterAgent(ctx, interfaces.Agent{
		ID: "agt_ag02", AgentTypeID: "hugr-data", ShortID: "ag02", Name: "t2",
	}))

	active, err := h.ListActiveSessions(ctx)
	require.NoError(t, err)
	assert.Empty(t, active)
}
