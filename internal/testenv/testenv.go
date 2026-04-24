//go:build duckdb_arrow

// Package testenv spins up an embedded hugr engine with hub.db
// attached for integration tests of every pkg/store/<domain>
// subpackage. Not imported from production code.
package testenv

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
//
// When EmbedderURL is non-empty, Engine registers an "embedding" data
// source pointing at that OpenAI-compatible endpoint; this is what
// lights up the `@embeddings` directive + `semantic:` / `summary:`
// arguments on session_events / memory_items. Leave it empty for
// tests that only exercise the no-embedder path.
type Opts struct {
	VectorDim     int
	EmbedderModel string
	EmbedderURL   string
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

	if o.EmbedderURL != "" && o.EmbedderModel != "" {
		path := fmt.Sprintf("%s?model=%s&timeout=30s", o.EmbedderURL, o.EmbedderModel)
		require.NoError(t, service.RegisterDataSource(ctx, types.DataSource{
			Name:    o.EmbedderModel,
			Type:    types.DataSourceType("embedding"),
			Prefix:  o.EmbedderModel,
			Path:    path,
			Sources: []types.CatalogSource{},
		}))
	}

	t.Cleanup(func() {
		_ = service.Close()
	})

	return service, hubPath
}

// EnvOrSkip returns the value of the named env variable; when unset
// (or empty) the test is skipped with a helpful message. Handles the
// common "we need a live local model for this test" pattern so
// individual tests don't repeat the check.
func EnvOrSkip(t *testing.T, name string) string {
	t.Helper()
	LoadDotEnv()
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		t.Skipf("%s not set; skipping test that requires a live local service", name)
	}
	return v
}

// LoadDotEnv walks upward from the current working directory looking
// for a `.env` file and os.Setenv's every KEY=VALUE it finds into the
// process environment (without overwriting already-set vars). Safe to
// call repeatedly — loads once per process. Mirrors what
// cmd/agent/main.go + pkg/config/LoadBootstrap do at boot time so
// tests pick up the same EMBED_LOCAL_URL / LLM_LOCAL_URL as prod.
func LoadDotEnv() {
	dotEnvOnce.Do(loadDotEnv)
}

var dotEnvOnce sync.Once

func loadDotEnv() {
	dir, err := os.Getwd()
	if err != nil {
		return
	}
	for i := 0; i < 8; i++ {
		candidate := filepath.Join(dir, ".env")
		if data, err := os.ReadFile(candidate); err == nil {
			applyEnvFile(string(data))
			return
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return
		}
		dir = parent
	}
}

func applyEnvFile(body string) {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		// Strip wrapping quotes (both single + double) to match viper's
		// .env handling.
		if len(val) >= 2 {
			first, last := val[0], val[len(val)-1]
			if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		if _, set := os.LookupEnv(key); set {
			continue
		}
		_ = os.Setenv(key, val)
	}
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
