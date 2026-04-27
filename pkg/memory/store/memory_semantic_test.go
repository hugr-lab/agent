//go:build duckdb_arrow

package store_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hugr-lab/hugen/internal/testenv"
	memstore "github.com/hugr-lab/hugen/pkg/memory/store"
)

// newEmbeddedClient wires a memstore.Client against an engine with a
// live embedder data source attached. Skips when EMBED_LOCAL_URL is
// not set in the environment (or .env). Model + dimension default to
// the LM Studio gemma-300m embedder the repo ships in config.yaml.
func newEmbeddedClient(t *testing.T) *memstore.Client {
	t.Helper()
	embedURL := testenv.EnvOrSkip(t, "EMBED_LOCAL_URL")
	model := "text-embedding-embeddinggemma-300m-qat"

	service, _ := testenv.Engine(t, testenv.Opts{
		VectorDim:     768,
		EmbedderModel: model,
		EmbedderURL:   embedURL,
	})

	logger := slog.New(slog.NewTextHandler(discardWriter{}, nil))
	testenv.RegisterAgent(t, service, "agt_ag01", "ag01", "embed-test-agent")

	c, err := memstore.New(service, memstore.Options{
		AgentID: "agt_ag01", AgentShort: "ag01", Logger: logger,
		EmbedderEnabled: true,
	})
	require.NoError(t, err)
	return c
}

// TestMemory_SemanticSearch seeds three facts from different domains
// and searches with a synonym of one of them. Hugr embeds both the
// stored content (via `summary:`) and the query (via `semantic:`);
// the semantically closest fact should top the results. Skipped
// when no live embedder is reachable.
//
// Spec 006 T411.
func TestMemory_SemanticSearch(t *testing.T) {
	h := newEmbeddedClient(t)
	ctx := context.Background()
	now := time.Now().UTC()
	validTo := now.Add(24 * time.Hour)

	type seed struct {
		id, content string
	}
	seeds := []seed{
		{"fact-schema", "tf.incidents table stores traffic incident reports with severity, coords, and timestamp"},
		{"fact-weather", "weather observations are keyed by station_id and include temperature, humidity, wind"},
		{"fact-query", "filter traffic incidents by bbox and date range using the hugr station_id FK"},
	}
	for _, s := range seeds {
		_, err := h.Store(ctx, memstore.Item{
			ID:         s.id,
			Content:    s.content,
			Category:   "schema",
			Volatility: "stable",
			Score:      0.5,
			ValidFrom:  now,
			ValidTo:    validTo,
		}, nil, nil)
		require.NoError(t, err, "seed %s", s.id)
	}

	// Query uses a synonym of the weather fact — should top the ranked
	// list ahead of the incident / query-template facts.
	res, err := h.Search(ctx, "barometric readings from meteorology sensors", memstore.SearchOpts{
		Limit: 3,
	})
	require.NoError(t, err)
	require.NotEmpty(t, res)
	assert.Equal(t, "fact-weather", res[0].ID,
		"semantic ordering: weather fact must top the weather-themed query (got %s)", res[0].ID)
}

// TestMemory_SearchFallbackOnEmptyQuery asserts the ILIKE / tag-only
// path still works when the query is empty — exercises the
// embedder-off branch end-to-end against a live engine.
//
// Spec 006 T412.
func TestMemory_SearchFallbackOnEmptyQuery(t *testing.T) {
	h := newEmbeddedClient(t)
	ctx := context.Background()
	now := time.Now().UTC()
	validTo := now.Add(24 * time.Hour)

	_, err := h.Store(ctx, memstore.Item{
		ID: "weather-fact", Content: "weather station obs",
		Category: "schema", Volatility: "stable", Score: 0.5,
		ValidFrom: now, ValidTo: validTo,
	}, []string{"weather", "schema"}, nil)
	require.NoError(t, err)

	_, err = h.Store(ctx, memstore.Item{
		ID: "traffic-fact", Content: "traffic incident table",
		Category: "schema", Volatility: "stable", Score: 0.5,
		ValidFrom: now, ValidTo: validTo,
	}, []string{"traffic", "schema"}, nil)
	require.NoError(t, err)

	// Empty query → no semantic branch. Tag filter picks the weather row.
	res, err := h.Search(ctx, "", memstore.SearchOpts{
		Tags:  []string{"weather"},
		Limit: 5,
	})
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.Equal(t, "weather-fact", res[0].ID)
}
