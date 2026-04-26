//go:build duckdb_arrow

package store_test

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/hugr-lab/hugen/internal/testenv"
	"github.com/hugr-lab/hugen/pkg/store/queries"
	"github.com/hugr-lab/query-engine/types"
)

// TestMain reuses one hugr engine across every approvals/store
// integration test in the package — engine bootstrap is the dominant
// cost (~20 s). Each fixture is responsible for clearing the shared
// agent's rows from approvals + tool_policies + sessions so tests
// don't see each other's writes.
func TestMain(m *testing.M) {
	code := m.Run()
	testenv.CloseShared()
	os.Exit(code)
}

// sharedFixtureMu serializes every test that uses the shared engine.
// Held for the LIFETIME of the calling test (Lock here, Unlock in
// t.Cleanup), so even if a test calls t.Parallel() the next test
// still waits for it to finish before its own Reset+setup proceeds.
var sharedFixtureMu sync.Mutex

// resetSharedTables wipes every row scoped to agent_id="agt_ag01"
// from the tables phase-4 store tests touch. Idempotent on empty.
// Called from each fixture builder right after acquiring the shared
// engine.
func resetSharedTables(t *testing.T, ctx context.Context, q types.Querier) error {
	t.Helper()
	sharedFixtureMu.Lock()
	t.Cleanup(sharedFixtureMu.Unlock)

	const op = `
		mutation Reset($agent: String!) {
			hub { db { agent {
				delete_approvals(filter: {agent_id: {eq: $agent}}) { affected_rows }
				delete_tool_policies(filter: {agent_id: {eq: $agent}}) { affected_rows }
				delete_session_events(filter: {agent_id: {eq: $agent}}) { affected_rows }
				delete_sessions(filter: {agent_id: {eq: $agent}}) { affected_rows }
			}}}
		}
	`
	return queries.RunMutation(ctx, q, op, map[string]any{"agent": "agt_ag01"})
}
