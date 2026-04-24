//go:build duckdb_arrow

package store_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	memstore "github.com/hugr-lab/hugen/pkg/memory/store"
	agentstore "github.com/hugr-lab/hugen/pkg/agent/store"
	"github.com/hugr-lab/hugen/internal/testenv"
)

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// newClient returns a memstore.Client + seeds the agent row (memory_items
// FK onto agents).
func newClient(t *testing.T, agentID, shortID string) *memstore.Client {
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
	c, err := memstore.New(service, memstore.Options{
		AgentID: agentID, AgentShort: shortID, Logger: logger,
	})
	require.NoError(t, err)
	return c
}

func TestMemory_StoreGetDelete(t *testing.T) {
	h := newClient(t, "agt_ag01", "ag01")
	ctx := context.Background()

	now := time.Now().UTC()
	fact := memstore.Item{
		Content:    "tf2.incidents has 14 fields",
		Category:   "schema",
		Volatility: "stable",
		Score:      0.8,
		Source:     "review:test",
		ValidFrom:  now,
		ValidTo:    now.Add(24 * time.Hour),
	}
	id, err := h.Store(ctx, fact, []string{"tf", "incidents", "schema"},
		[]memstore.Link{{TargetID: "other-fact", Relation: "uses"}})
	require.NoError(t, err)
	require.NotEmpty(t, id)

	got, err := h.Get(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, fact.Content, got.Content)
	assert.Equal(t, "schema", got.Category)
	assert.ElementsMatch(t, []string{"tf", "incidents", "schema"}, got.Tags)
	assert.ElementsMatch(t, []string{"other-fact"}, got.Links)
	assert.True(t, got.IsValid)

	res, err := h.Search(ctx, "incidents", memstore.SearchOpts{Limit: 5})
	require.NoError(t, err)
	require.NotEmpty(t, res)
	assert.Equal(t, id, res[0].ID)

	res, err = h.Search(ctx, "", memstore.SearchOpts{Tags: []string{"tf"}, Limit: 5})
	require.NoError(t, err)
	require.NotEmpty(t, res)

	res, err = h.Search(ctx, "", memstore.SearchOpts{Tags: []string{"weather"}, Limit: 5})
	require.NoError(t, err)
	assert.Empty(t, res)

	require.NoError(t, h.Delete(ctx, id))
	got, err = h.Get(ctx, id)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestMemory_Reinforce(t *testing.T) {
	h := newClient(t, "agt_ag01", "ag01")
	ctx := context.Background()
	now := time.Now().UTC()

	id, err := h.Store(ctx, memstore.Item{
		Content: "query pattern: filter weather by region", Category: "query_template",
		Volatility: "stable", Score: 0.5,
		ValidFrom: now, ValidTo: now.Add(24 * time.Hour),
	}, []string{"weather"}, nil)
	require.NoError(t, err)

	require.NoError(t, h.Reinforce(ctx, id, 0.3, []string{"region"},
		[]memstore.Link{{TargetID: "schema-weather", Relation: "uses"}}))

	got, err := h.Get(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.InDelta(t, 0.8, got.Score, 0.001)
	assert.Contains(t, got.Tags, "weather")
	assert.Contains(t, got.Tags, "region")
	assert.Contains(t, got.Links, "schema-weather")
}

func TestMemory_Stats(t *testing.T) {
	h := newClient(t, "agt_ag01", "ag01")
	ctx := context.Background()
	now := time.Now().UTC()

	_, err := h.Store(ctx, memstore.Item{
		Content: "a", Category: "schema", Volatility: "stable", Score: 0.5,
		ValidFrom: now, ValidTo: now.Add(24 * time.Hour),
	}, nil, nil)
	require.NoError(t, err)
	_, err = h.Store(ctx, memstore.Item{
		Content: "b", Category: "schema", Volatility: "stable", Score: 0.5,
		ValidFrom: now, ValidTo: now.Add(24 * time.Hour),
	}, nil, nil)
	require.NoError(t, err)
	_, err = h.Store(ctx, memstore.Item{
		Content: "c", Category: "query_template", Volatility: "stable", Score: 0.5,
		ValidFrom: now, ValidTo: now.Add(24 * time.Hour),
	}, nil, nil)
	require.NoError(t, err)

	stats, err := h.Stats(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, stats.TotalItems)
	assert.Equal(t, 3, stats.ActiveItems)
	assert.Equal(t, 2, stats.ByCategory["schema"])
	assert.Equal(t, 1, stats.ByCategory["query_template"])

	hint, err := h.Hint(ctx, "", nil)
	require.NoError(t, err)
	assert.Contains(t, hint, "3 long-term facts")
}

func TestMemory_DeleteExpired(t *testing.T) {
	h := newClient(t, "agt_ag01", "ag01")
	ctx := context.Background()
	now := time.Now().UTC()

	_, err := h.Store(ctx, memstore.Item{
		Content: "expired", Category: "schema", Volatility: "volatile", Score: 0.5,
		ValidFrom: now.Add(-48 * time.Hour), ValidTo: now.Add(-1 * time.Hour),
	}, nil, nil)
	require.NoError(t, err)
	freshID, err := h.Store(ctx, memstore.Item{
		Content: "fresh", Category: "schema", Volatility: "stable", Score: 0.5,
		ValidFrom: now, ValidTo: now.Add(24 * time.Hour),
	}, nil, nil)
	require.NoError(t, err)

	_, err = h.DeleteExpired(ctx)
	require.NoError(t, err)

	got, err := h.Get(ctx, freshID)
	require.NoError(t, err)
	require.NotNil(t, got)

	res, err := h.Search(ctx, "expired", memstore.SearchOpts{Limit: 5})
	require.NoError(t, err)
	assert.Empty(t, res)
}
