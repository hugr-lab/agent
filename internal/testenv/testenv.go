//go:build duckdb_arrow

// Package testenv spins up an embedded hugr engine with hub.db
// attached for integration tests of every pkg/store/<domain>
// subpackage. Not imported from production code.
package testenv

import (
	"context"
	"path/filepath"
	"testing"

	hugr "github.com/hugr-lab/query-engine"
	"github.com/hugr-lab/query-engine/pkg/auth"
	coredb "github.com/hugr-lab/query-engine/pkg/data-sources/sources/runtime/core-db"
	"github.com/hugr-lab/query-engine/pkg/db"
	"github.com/hugr-lab/query-engine/types"
	"github.com/stretchr/testify/require"

	"github.com/hugr-lab/hugen/pkg/store/local"
	"github.com/hugr-lab/hugen/pkg/store/local/migrate"
)

// Opts tunes Engine for tests that need vector search.
type Opts struct {
	VectorDim     int
	EmbedderModel string
}

// Engine spins up an embedded hugr engine with hub.db attached, seeded
// with agent_type="hugr-data" + agent={agt_ag01/ag01/hugr-data-agent-test}.
// Returns the service + hub.db filesystem path.
func Engine(t *testing.T, opt ...Opts) (*hugr.Service, string) {
	t.Helper()
	ctx := context.Background()

	var o Opts
	if len(opt) > 0 {
		o = opt[0]
	}

	dir := t.TempDir()
	hubPath := filepath.Join(dir, "memory.db")

	require.NoError(t, migrate.Ensure(migrate.Config{
		Path:       hubPath,
		VectorSize: o.VectorDim,
		Seed: &migrate.SeedData{
			AgentType: migrate.SeedAgentType{
				ID:          "hugr-data",
				Name:        "Hugr Data Agent",
				Description: "Default agent type for tests",
				Config:      map[string]any{"constitution": "test"},
			},
			Agent: migrate.SeedAgent{
				ID:      "agt_ag01",
				ShortID: "ag01",
				Name:    "hugr-data-agent-test",
			},
		},
	}))

	source := local.NewSource(local.SourceConfig{
		Path:          hubPath,
		VectorSize:    o.VectorDim,
		EmbedderModel: o.EmbedderModel,
	})
	service, err := hugr.New(hugr.Config{
		DB:     db.Config{},
		CoreDB: coredb.New(coredb.Config{VectorSize: 0}),
		Auth:   &auth.Config{},
	})
	require.NoError(t, err)
	require.NoError(t, service.AttachRuntimeSource(ctx, source))
	require.NoError(t, service.Init(ctx))

	t.Cleanup(func() {
		_ = service.Close()
	})

	return service, hubPath
}

// MustQuery runs a GraphQL query and fails the test on transport or
// GraphQL errors. The returned Response is auto-closed at test
// teardown.
func MustQuery(t *testing.T, q types.Querier, query string, vars map[string]any) *types.Response {
	t.Helper()
	resp, err := q.Query(context.Background(), query, vars)
	require.NoError(t, err, "query failed: %s", query)
	if resp != nil {
		t.Cleanup(resp.Close)
	}
	require.NoError(t, resp.Err(), "graphql errors: %v\nquery: %s", resp.Errors, query)
	return resp
}
