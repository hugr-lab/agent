//go:build duckdb_arrow

package artifacts

import (
	"context"
	"os"
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
func ResetSharedTables(ctx context.Context, q types.Querier) error {
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
