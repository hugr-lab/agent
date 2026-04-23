//go:build duckdb_arrow

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	memstore "github.com/hugr-lab/hugen/pkg/memory/store"
)

// store returns a reusable item helper.
func seedItem(t *testing.T, h *memstore.Client, content, category string, score float64) string {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	id, err := h.Store(ctx, memstore.Item{
		Content: content, Category: category, Volatility: "stable",
		Score: score, Source: "test",
		ValidFrom: now, ValidTo: now.Add(24 * time.Hour),
	}, nil, nil)
	require.NoError(t, err)
	return id
}

func TestMemory_Supersede(t *testing.T) {
	h := newClient(t, "agt_ag01", "ag01")
	// memory_log rows require a session id on the context so the
	// logBatch FK guard lets them through. Any non-empty id works.
	ctx := memstore.WithSessionID(context.Background(), "sess-supersede")
	now := time.Now().UTC()

	oldID := seedItem(t, h, "old fact", "schema", 0.5)
	oldGot, err := h.Get(ctx, oldID)
	require.NoError(t, err)
	require.NotNil(t, oldGot)
	require.True(t, oldGot.IsValid)

	newID, err := h.Supersede(ctx, oldID, memstore.Item{
		Content: "newer fact", Category: "schema", Volatility: "stable",
		Score: 0.9, Source: "review:supersede",
		ValidFrom: now, ValidTo: now.Add(48 * time.Hour),
	}, []string{"schema"}, nil)
	require.NoError(t, err)
	assert.NotEmpty(t, newID)
	assert.NotEqual(t, oldID, newID)

	// Old item is gone from the active set.
	oldGot, err = h.Get(ctx, oldID)
	require.NoError(t, err)
	assert.Nil(t, oldGot, "superseded item should no longer be retrievable")

	// New item present.
	newGot, err := h.Get(ctx, newID)
	require.NoError(t, err)
	require.NotNil(t, newGot)
	assert.Equal(t, "newer fact", newGot.Content)

	// Link from new → old ("supersedes") survives round-trip.
	assert.Contains(t, newGot.Links, oldID,
		"new item should carry a supersedes link to the old id")

	// memory_log records the supersede transition (session id was
	// attached via WithSessionID above, so the FK guard passes).
	log, err := h.GetLog(ctx, oldID, 10)
	require.NoError(t, err)
	var foundSupersede bool
	for _, e := range log {
		if e.EventType == "supersede" {
			foundSupersede = true
			assert.Equal(t, newID, e.Details["superseded_by"])
		}
	}
	assert.True(t, foundSupersede, "supersede event missing from memory_log")
}

func TestMemory_GetLinked(t *testing.T) {
	h := newClient(t, "agt_ag01", "ag01")
	ctx := context.Background()
	now := time.Now().UTC()

	// Leaf first so B's link target exists.
	aID := seedItem(t, h, "schema-a", "schema", 0.7)

	bID, err := h.Store(ctx, memstore.Item{
		Content: "query-template-b", Category: "query_template",
		Volatility: "stable", Score: 0.6, Source: "test",
		ValidFrom: now, ValidTo: now.Add(24 * time.Hour),
	}, nil, []memstore.Link{{TargetID: aID, Relation: "uses"}})
	require.NoError(t, err)

	// Sanity: Get(B).Links contains A. If this fails the Store/Get
	// path, not GetLinked, is the problem.
	bGot, err := h.Get(ctx, bID)
	require.NoError(t, err)
	require.NotNil(t, bGot)
	require.Contains(t, bGot.Links, aID, "Store must persist outgoing links")

	// GetLinked's semantics: depth=1 walks the seed's outgoing edges
	// but the seed itself is excluded from the result set AND the
	// walk terminates before visiting the targets. So neighbours
	// show up at depth >= 2. Documented here because it's easy to
	// misread as "hop count".
	empty, err := h.GetLinked(ctx, bID, 1)
	require.NoError(t, err)
	assert.Empty(t, empty, "depth=1 returns no rows (seed excluded, walk stops)")

	linked, err := h.GetLinked(ctx, bID, 2)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, r := range linked {
		ids[r.ID] = true
	}
	assert.True(t, ids[aID], "depth=2 from B must include A")
}

func TestMemory_AddRemoveTags(t *testing.T) {
	h := newClient(t, "agt_ag01", "ag01")
	ctx := context.Background()
	id := seedItem(t, h, "tag-target", "schema", 0.5)

	require.NoError(t, h.AddTags(ctx, id, []string{"alpha", "beta"}))
	got, err := h.Get(ctx, id)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"alpha", "beta"}, got.Tags)

	require.NoError(t, h.RemoveTags(ctx, id, []string{"alpha"}))
	got, err = h.Get(ctx, id)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"beta"}, got.Tags)
}

func TestMemory_AddRemoveLink(t *testing.T) {
	h := newClient(t, "agt_ag01", "ag01")
	ctx := context.Background()
	src := seedItem(t, h, "src", "schema", 0.5)
	tgt := seedItem(t, h, "tgt", "schema", 0.5)

	require.NoError(t, h.AddLink(ctx, memstore.Link{
		SourceID: src, TargetID: tgt, Relation: "uses",
	}))

	got, err := h.Get(ctx, src)
	require.NoError(t, err)
	assert.Contains(t, got.Links, tgt)

	require.NoError(t, h.RemoveLink(ctx, src, tgt))
	got, err = h.Get(ctx, src)
	require.NoError(t, err)
	assert.NotContains(t, got.Links, tgt)
}

// TestMemory_Log_ListLog verifies the audit log write path + filter
// by memory_item_id, which the learning reviewer uses as cursor.
func TestMemory_Log_ListLog(t *testing.T) {
	h := newClient(t, "agt_ag01", "ag01")
	ctx := context.Background()
	memID := seedItem(t, h, "log-target", "schema", 0.5)

	// Record a retrieve event.
	require.NoError(t, h.Log(ctx, memstore.LogEntry{
		MemoryItemID: memID,
		SessionID:    "sess-x",
		EventType:    "retrieve",
		Details:      map[string]any{"reason": "test"},
	}))

	// Listing by session_id surfaces the retrieve entry written above.
	entries, err := h.ListLog(ctx, memstore.ListLogOpts{
		SessionID: "sess-x", Limit: 10,
	})
	require.NoError(t, err)
	require.NotEmpty(t, entries, "expected retrieve entry filtered by session")

	// GetLog keyed on the memory_item surfaces the retrieve row we
	// just wrote. Store() itself doesn't emit a memory_log entry —
	// that's reserved for Reinforce/Supersede audit transitions.
	quick, err := h.GetLog(ctx, memID, 10)
	require.NoError(t, err)
	require.NotEmpty(t, quick)
	assert.Equal(t, "retrieve", quick[0].EventType)
}

// TestMemory_Hint always returns a stats-rendered summary string
// ("N long-term facts [...by category]"). Coverage verifies the
// call path + that seeding new items is reflected.
func TestMemory_Hint(t *testing.T) {
	h := newClient(t, "agt_ag01", "ag01")
	ctx := context.Background()

	empty, err := h.Hint(ctx, "anything", nil)
	require.NoError(t, err)
	assert.Contains(t, empty, "long-term facts")

	seedItem(t, h, "hint target: incidents table schema", "schema", 0.9)
	seedItem(t, h, "hint target: facilities table schema", "schema", 0.8)

	hint, err := h.Hint(ctx, "incidents", nil)
	require.NoError(t, err)
	assert.Contains(t, hint, "schema", "hint should mention the seeded category")
}
