//go:build duckdb_arrow

package store_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentstore "github.com/hugr-lab/hugen/pkg/agent/store"
	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
	"github.com/hugr-lab/hugen/pkg/store/testenv"
)

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func newClient(t *testing.T, agentID, shortID string) *sessstore.Client {
	t.Helper()
	service, _ := testenv.Engine(t)
	logger := slog.New(slog.NewTextHandler(discardWriter{}, nil))
	reg, err := agentstore.New(service, agentstore.Options{
		AgentID: agentID, AgentShort: shortID, Logger: logger,
	})
	require.NoError(t, err)
	require.NoError(t, reg.RegisterAgent(context.Background(), agentstore.Agent{
		ID: agentID, AgentTypeID: "hugr-data", ShortID: shortID,
		Name: "test-agent", Status: "active",
	}))
	c, err := sessstore.New(service, sessstore.Options{
		AgentID: agentID, AgentShort: shortID, Logger: logger,
	})
	require.NoError(t, err)
	return c
}

func TestSessions_CRUD(t *testing.T) {
	h := newClient(t, "agt_ag01", "ag01")
	ctx := context.Background()

	_, err := h.CreateSession(ctx, sessstore.Record{
		ID: "sess-active", OwnerID: "u1", Status: "active", Mission: "active one",
	})
	require.NoError(t, err)
	_, err = h.CreateSession(ctx, sessstore.Record{
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

	_, err = h.AppendEvent(ctx, sessstore.Event{
		SessionID: "sess-active", EventType: sessstore.EventTypeSkillLoaded,
		Author: "sess-active", Content: "hugr-data",
		Metadata: map[string]any{"skill": "hugr-data"},
	})
	require.NoError(t, err)

	_, err = h.AppendEvent(ctx, sessstore.Event{
		SessionID: "sess-active", EventType: sessstore.EventTypeSkillUnloaded,
		Author: "sess-active", Content: "hugr-data",
		Metadata: map[string]any{"skill": "hugr-data"},
	})
	require.NoError(t, err)

	evs, err := h.GetEvents(ctx, "sess-active")
	require.NoError(t, err)
	require.Len(t, evs, 2)
	assert.Equal(t, sessstore.EventTypeSkillLoaded, evs[0].EventType)
	assert.Equal(t, sessstore.EventTypeSkillUnloaded, evs[1].EventType)
	assert.Equal(t, 1, evs[0].Seq)
	assert.Equal(t, 2, evs[1].Seq)
	assert.Equal(t, "hugr-data", evs[0].Content)
}

func TestSessions_Notes(t *testing.T) {
	h := newClient(t, "agt_ag01", "ag01")
	ctx := context.Background()
	_, err := h.CreateSession(ctx, sessstore.Record{
		ID: "sess-notes", OwnerID: "u1", Status: "active",
	})
	require.NoError(t, err)

	id1, err := h.AddNote(ctx, sessstore.Note{
		SessionID: "sess-notes", Content: "found 14 fields",
	})
	require.NoError(t, err)
	require.NotEmpty(t, id1)
	_, err = h.AddNote(ctx, sessstore.Note{
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
	h := newClient(t, "agt_ag01", "ag01")
	ctx := context.Background()
	_, err := h.CreateSession(ctx, sessstore.Record{
		ID: "sess-p", OwnerID: "u1", Status: "active",
	})
	require.NoError(t, err)

	require.NoError(t, h.AddParticipant(ctx, sessstore.Participant{
		SessionID: "sess-p", UserID: "u1", Role: "owner",
	}))
	require.NoError(t, h.AddParticipant(ctx, sessstore.Participant{
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
	h := newClient(t, "agt_ag02", "ag02")
	ctx := context.Background()
	active, err := h.ListActiveSessions(ctx)
	require.NoError(t, err)
	assert.Empty(t, active)
}
