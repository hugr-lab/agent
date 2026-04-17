//go:build duckdb_arrow

package hubdb_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hugr-lab/hugen/adapters/hubdb"
	"github.com/hugr-lab/hugen/interfaces"
)

func newTestHubDB(t *testing.T, agentID, shortID string) interfaces.HubDB {
	t.Helper()
	service, _ := testEngine(t)
	h, err := hubdb.New(service, hubdb.Options{
		AgentID:    agentID,
		AgentShort: shortID,
		Logger:     slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	})
	require.NoError(t, err)
	return h
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestGetAgentType_Seeded(t *testing.T) {
	h := newTestHubDB(t, "agt_ag01", "ag01")

	got, err := h.GetAgentType(context.Background(), "hugr-data")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "hugr-data", got.ID)
	assert.NotEmpty(t, got.Name)
}

func TestGetAgentType_NotFound(t *testing.T) {
	h := newTestHubDB(t, "agt_ag01", "ag01")
	got, err := h.GetAgentType(context.Background(), "does-not-exist")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestRegisterAgent_FirstTime(t *testing.T) {
	h := newTestHubDB(t, "agt_ag01", "ag01")
	ctx := context.Background()

	err := h.RegisterAgent(ctx, interfaces.Agent{
		ID:          "agt_ag02",
		AgentTypeID: "hugr-data",
		ShortID:     "ag02",
		Name:        "second agent",
	})
	require.NoError(t, err)

	got, err := h.GetAgent(ctx, "agt_ag02")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "ag02", got.ShortID)
	assert.Equal(t, "active", got.Status)
}

func TestRegisterAgent_Idempotent(t *testing.T) {
	h := newTestHubDB(t, "agt_ag01", "ag01")
	ctx := context.Background()

	first, err := h.GetAgent(ctx, "agt_ag01")
	require.NoError(t, err)
	require.NotNil(t, first)
	originalCreatedAt := first.CreatedAt

	// Second register refreshes config_override + last_active, keeps created_at.
	require.NoError(t, h.RegisterAgent(ctx, interfaces.Agent{
		ID:             "agt_ag01",
		AgentTypeID:    "hugr-data",
		ShortID:        "ag01",
		Name:           "hugr-data-agent-test",
		ConfigOverride: map[string]any{"llm": map[string]any{"model": "gemini-pro-3-1"}},
	}))

	again, err := h.GetAgent(ctx, "agt_ag01")
	require.NoError(t, err)
	require.NotNil(t, again)
	assert.True(t, again.CreatedAt.Equal(originalCreatedAt),
		"created_at should be stable, got %v want %v", again.CreatedAt, originalCreatedAt)
	require.NotNil(t, again.ConfigOverride["llm"])
	llm := again.ConfigOverride["llm"].(map[string]any)
	assert.Equal(t, "gemini-pro-3-1", llm["model"])
}

func TestGetAgent_NotFound(t *testing.T) {
	h := newTestHubDB(t, "agt_ag01", "ag01")
	got, err := h.GetAgent(context.Background(), "nobody")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestUpdateAgentActivity(t *testing.T) {
	h := newTestHubDB(t, "agt_ag01", "ag01")
	ctx := context.Background()

	before, err := h.GetAgent(ctx, "agt_ag01")
	require.NoError(t, err)

	require.NoError(t, h.UpdateAgentActivity(ctx, "agt_ag01"))

	after, err := h.GetAgent(ctx, "agt_ag01")
	require.NoError(t, err)
	assert.False(t, after.LastActive.Before(before.LastActive),
		"last_active should move forward or stay equal: before=%v after=%v", before.LastActive, after.LastActive)
}
