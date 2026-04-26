//go:build duckdb_arrow

package artifacts

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/hugr-lab/hugen/internal/testenv"
	"github.com/hugr-lab/hugen/pkg/store/queries"
	"github.com/hugr-lab/query-engine/types"
)

// TestMain reuses one hugr engine across every integration test in
// the package — engine bootstrap is the dominant cost (~20 s). Each
// fixture is responsible for clearing the shared agent's rows from
// the artifact + session tables (resetSharedTables below) so tests
// don't see each other's writes.
func TestMain(m *testing.M) {
	code := m.Run()
	testenv.CloseShared()
	os.Exit(code)
}

// sharedFixtureMu serializes every test that uses the shared
// engine. Without this lock, two tests using ResetSharedTables in
// parallel would wipe each other's data mid-publish — the shared
// engine has zero per-test isolation. The mutex is held for the
// LIFETIME of the calling test (Lock here, Unlock in t.Cleanup),
// so even if a test calls t.Parallel() the next test still waits
// for it to finish before its own Reset+setup can proceed.
var sharedFixtureMu sync.Mutex

// ResetSharedTables wipes every row scoped to agent_id="agt_ag01"
// from the tables the artifact tests touch. Idempotent on empty.
// Called from each fixture builder right after acquiring the shared
// engine. Exported so the artifacts_test sibling package can call
// it.
//
// All mutations live under hub.db.agent (the engine renders the
// per-agent mutation surface there); fields the schema doesn't
// expose at all are skipped — running the same mutation against a
// pristine engine is harmless.
//
// The function takes *testing.T (rather than just context.Context)
// because it needs to register a t.Cleanup to release
// sharedFixtureMu after the test completes. Passing t also gives
// us t.Helper() for clean stack traces.
func ResetSharedTables(t *testing.T, ctx context.Context, q types.Querier) error {
	t.Helper()
	sharedFixtureMu.Lock()
	t.Cleanup(sharedFixtureMu.Unlock)

	const op = `
		mutation Reset($agent: String!) {
			hub { db { agent {
				delete_artifact_grants(filter: {agent_id: {eq: $agent}}) { affected_rows }
				delete_artifacts(filter: {agent_id: {eq: $agent}}) { affected_rows }
				delete_session_notes(filter: {agent_id: {eq: $agent}}) { affected_rows }
				delete_session_events(filter: {agent_id: {eq: $agent}}) { affected_rows }
				delete_sessions(filter: {agent_id: {eq: $agent}}) { affected_rows }
			}}}
		}
	`
	return queries.RunMutation(ctx, q, op, map[string]any{"agent": "agt_ag01"})
}
