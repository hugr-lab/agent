//go:build duckdb_arrow

package migrate_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hugr-lab/hugen/adapters/hubdb/migrate"
)

func TestEnsure_Fresh(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, migrate.Ensure(migrate.Config{
		Path:          filepath.Join(dir, "memory.db"),
		VectorSize:    384,
		EmbedderModel: "gemma-embedding",
		Seed: &migrate.SeedData{
			AgentType: migrate.SeedAgentType{ID: "hugr-data", Name: "X", Config: map[string]any{}},
			Agent:     migrate.SeedAgent{ID: "agt_ag01", ShortID: "ag01", Name: "x"},
		},
	}))
}

func TestEnsure_DimMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	require.NoError(t, migrate.Ensure(migrate.Config{
		Path: path, VectorSize: 384, EmbedderModel: "gemma-embedding",
		Seed: &migrate.SeedData{
			AgentType: migrate.SeedAgentType{ID: "hugr-data", Name: "X", Config: map[string]any{}},
			Agent:     migrate.SeedAgent{ID: "agt_ag01", ShortID: "ag01", Name: "x"},
		},
	}))

	err := migrate.Ensure(migrate.Config{Path: path, VectorSize: 768, EmbedderModel: "gemma-embedding"})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "embedding dimension mismatch"),
		"expected dimension-mismatch error, got: %v", err)
}

func TestEnsure_ModelMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	require.NoError(t, migrate.Ensure(migrate.Config{
		Path: path, VectorSize: 384, EmbedderModel: "gemma-embedding",
		Seed: &migrate.SeedData{
			AgentType: migrate.SeedAgentType{ID: "hugr-data", Name: "X", Config: map[string]any{}},
			Agent:     migrate.SeedAgent{ID: "agt_ag01", ShortID: "ag01", Name: "x"},
		},
	}))

	err := migrate.Ensure(migrate.Config{Path: path, VectorSize: 384, EmbedderModel: "other-embedding"})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "embedding model mismatch"),
		"expected model-mismatch error, got: %v", err)
}

func TestEnsure_MatchingConfigOK(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	cfg := migrate.Config{
		Path: path, VectorSize: 384, EmbedderModel: "gemma-embedding",
		Seed: &migrate.SeedData{
			AgentType: migrate.SeedAgentType{ID: "hugr-data", Name: "X", Config: map[string]any{}},
			Agent:     migrate.SeedAgent{ID: "agt_ag01", ShortID: "ag01", Name: "x"},
		},
	}
	require.NoError(t, migrate.Ensure(cfg))
	// Second run with the same config is a no-op (schema up to date).
	cfg.Seed = nil
	require.NoError(t, migrate.Ensure(cfg))
}
