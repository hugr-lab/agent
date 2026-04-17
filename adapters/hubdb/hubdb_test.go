//go:build duckdb_arrow

package hubdb_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hugr-lab/hugen/adapters/hubdb"
)

func TestHubDB_New_RequiresAgentID(t *testing.T) {
	service, _ := testEngine(t)
	_, err := hubdb.New(service, hubdb.Options{})
	require.Error(t, err)
}

func TestHubDB_New_Defaults(t *testing.T) {
	service, _ := testEngine(t)
	h, err := hubdb.New(service, hubdb.Options{
		AgentID:    "agt_ag01",
		AgentShort: "ag01",
	})
	require.NoError(t, err)
	assert.Equal(t, "agt_ag01", h.AgentID())
	assert.Equal(t, 0, h.Dimension())
	assert.False(t, h.Available())

	// Embed with no model → disabled, not a transport error.
	_, err = h.Embed(context.Background(), "hello")
	require.Error(t, err)
	assert.True(t, errors.Is(err, hubdb.ErrEmbeddingDisabled))

	// Close is idempotent.
	require.NoError(t, h.Close())
	require.NoError(t, h.Close())
}

func TestHubDB_Dimension_WithModel(t *testing.T) {
	service, _ := testEngine(t)
	h, err := hubdb.New(service, hubdb.Options{
		AgentID:        "agt_ag01",
		AgentShort:     "ag01",
		Dimension:      768,
		EmbeddingModel: "gemma-embedding",
		Logger:         slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	})
	require.NoError(t, err)
	assert.Equal(t, 768, h.Dimension())
	// Available reports only config — the actual probe is done by
	// setupLocalSources at startup and is not re-run per HubDB call.
	assert.True(t, h.Available())
}
