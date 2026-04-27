//go:build duckdb_arrow

package executor_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hugr-lab/hugen/internal/testenv"
	"github.com/hugr-lab/hugen/pkg/missions/executor"
	"github.com/hugr-lab/hugen/pkg/missions/graph"
	missionsstore "github.com/hugr-lab/hugen/pkg/missions/store"
	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
)

// restoreFixture builds a fresh hub.db + store + sessstore client for
// tests that pre-seed mission rows directly (bypassing the executor's
// own Register path) to simulate a prior agent run.
type restoreFixture struct {
	store *missionsstore.Store
	sess  *sessstore.Client
	coord string
}

func newRestoreFixture(t *testing.T) *restoreFixture {
	t.Helper()
	service, _ := testenv.Engine(t)
	logger := slog.New(slog.NewTextHandler(discardWriter{}, nil))
	testenv.RegisterAgent(t, service, "agt_ag01", "ag01", "test-agent")

	sess, err := sessstore.New(service, sessstore.Options{
		AgentID: "agt_ag01", AgentShort: "ag01", Logger: logger,
	})
	require.NoError(t, err)

	_, err = sess.CreateSession(context.Background(), sessstore.Record{
		ID: "coord-restore", AgentID: "agt_ag01", Status: "active",
		SessionType: sessstore.SessionTypeRoot,
	})
	require.NoError(t, err)

	return &restoreFixture{
		store: missionsstore.New(sess, service, logger),
		sess:  sess,
		coord: "coord-restore",
	}
}

// seedMission writes one sub-agent row + its first user_message event,
// emulating a mission that started in a prior run. Returns the
// mission id for follow-up assertions.
func (f *restoreFixture) seedMission(
	t *testing.T,
	id, role, task, status string,
	dependsOn []string,
	lastEventOffset time.Duration,
) string {
	t.Helper()
	meta := map[string]any{
		graph.MetadataKeySkill:        "x",
		graph.MetadataKeyRole:         role,
		graph.MetadataKeyCoordSession: f.coord,
	}
	if len(dependsOn) > 0 {
		meta[graph.MetadataKeyDependsOn] = dependsOn
	}
	_, err := f.sess.CreateSession(context.Background(), sessstore.Record{
		ID:              id,
		AgentID:         "agt_ag01",
		ParentSessionID: f.coord,
		SessionType:     sessstore.SessionTypeSubAgent,
		Status:          status,
		Mission:         task,
		Metadata:        meta,
	})
	require.NoError(t, err)
	if status == "active" {
		// One event so LastEventAt has something to read.
		_, err := f.sess.AppendEvent(context.Background(), sessstore.Event{
			SessionID: id,
			AgentID:   "agt_ag01",
			EventType: sessstore.EventTypeUserMessage,
			Author:    "user",
			Content:   "task: " + task,
		})
		require.NoError(t, err)
		// Backdate the event by lastEventOffset (positive means "older").
		// We do this with a direct SQL update because hub.db's
		// session_events.created_at is server-stamped.
		// Skip — the test fixture infra exposes no time-travel hook,
		// so we rely on lastEventOffset of 0 (= just-now) for fresh
		// resumes and use the synthetic-spawn-event path for staleness.
		_ = lastEventOffset
	}
	return id
}

// freshExec builds a new Executor on top of f's store/sess so each
// test exercises a fresh in-memory DAG.
func (f *restoreFixture) freshExec(t *testing.T) *executor.Executor {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(testWriter{t: t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	driver := newFakeDriver(f.sess)
	return executor.New(executor.Config{
		Store:       f.store,
		Events:      f.sess,
		Driver:      driver,
		Logger:      logger,
		Parallelism: 4,
	})
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}

// ---- Tests -------------------------------------------------------

func TestExecutor_RestoreState_AbandonsActiveOnRestart(t *testing.T) {
	f := newRestoreFixture(t)
	ctx := context.Background()

	// Any non-terminal sub-agent row at boot time is a casualty of the
	// previous process — the LLM turn doesn't survive process death,
	// so Restore reaps it cleanly with reason="restart: process died".
	id := f.seedMission(t, "sub-stale", "y", "stalled", "active", nil, 0)

	exec := f.freshExec(t)
	rep, err := exec.RestoreState(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, rep.StaleAbandoned)
	assert.Equal(t, 0, rep.Resumed)

	// Persisted status flipped to abandoned.
	ms, err := f.store.ListMissions(ctx, f.coord, "")
	require.NoError(t, err)
	require.Len(t, ms, 1)
	assert.Equal(t, graph.StatusAbandoned, ms[0].Status)
	assert.Equal(t, id, ms[0].ID)

	// Coordinator got exactly one agent_result event with status=abandoned
	// and reason="restart: stale" (one per stale mission).
	events, err := f.sess.GetEvents(ctx, f.coord)
	require.NoError(t, err)
	results := 0
	for _, ev := range events {
		if ev.EventType != sessstore.EventTypeAgentResult {
			continue
		}
		mid, _ := ev.Metadata["mission_id"].(string)
		if mid != id {
			continue
		}
		results++
		assert.Equal(t, "abandoned", ev.Metadata["status"])
		assert.Equal(t, "restart: process died", ev.Metadata["reason"])
	}
	assert.Equal(t, 1, results)
}

func TestExecutor_RestoreState_NoDuplicateSpawn(t *testing.T) {
	f := newRestoreFixture(t)
	ctx := context.Background()

	// Pre-seed: a mission already running with its mission_spawn event
	// already on the coordinator, simulating a crash mid-run.
	id := f.seedMission(t, "sub-fresh", "y", "alpha", "active", nil, 0)
	_, err := f.sess.AppendEvent(ctx, sessstore.Event{
		SessionID: f.coord,
		AgentID:   "agt_ag01",
		EventType: sessstore.EventTypeAgentSpawn,
		Author:    "agent",
		Content:   "Spawning x/y: alpha",
		Metadata:  map[string]any{"mission_id": id},
	})
	require.NoError(t, err)

	// Restore must not duplicate the existing mission_spawn event;
	// active rows get abandoned, not re-spawned.
	exec := f.freshExec(t)
	rep, err := exec.RestoreState(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, rep.Resumed, "restart abandons active rows, never resumes")
	assert.Equal(t, 1, rep.StaleAbandoned)

	events, err := f.sess.GetEvents(ctx, f.coord)
	require.NoError(t, err)
	spawns := 0
	for _, ev := range events {
		if ev.EventType == sessstore.EventTypeAgentSpawn {
			if mid, _ := ev.Metadata["mission_id"].(string); mid == id {
				spawns++
			}
		}
	}
	assert.Equal(t, 1, spawns, "no duplicate mission_spawn after restore")
}

func TestExecutor_RestoreState_ReadyPromotion(t *testing.T) {
	f := newRestoreFixture(t)
	ctx := context.Background()

	// Pre-seed: upstream completed before crash, downstream still
	// pending. Restore should bump downstream → ready.
	upID := f.seedMission(t, "sub-up", "u", "upstream", "completed", nil, 0)
	downID := f.seedMission(t, "sub-down", "d", "downstream", "active", []string{upID}, 0)

	exec := f.freshExec(t)
	rep, err := exec.RestoreState(ctx)
	require.NoError(t, err)

	// Under the "abandon all in-flight on restart" policy, active
	// rows count as StaleAbandoned, not Resumed. The terminal upstream
	// stays Done; the downstream lands Abandoned with the restart
	// reason.
	assert.Equal(t, 0, rep.Resumed)
	assert.Equal(t, 1, rep.StaleAbandoned)

	snapshot := exec.Snapshot(ctx, f.coord)
	statuses := map[string]string{}
	for _, n := range snapshot {
		statuses[n.ID] = n.Status
	}
	assert.Equal(t, graph.StatusDone, statuses[upID], "upstream stays done")
	assert.Equal(t, graph.StatusAbandoned, statuses[downID], "downstream reaped on restart")
}

func TestExecutor_RestoreState_CascadeOnTerminalUpstream(t *testing.T) {
	f := newRestoreFixture(t)
	ctx := context.Background()

	// Upstream failed before crash; an `active` downstream that didn't
	// get the cascade should be abandoned by Restore with reason
	// "restart: upstream terminal".
	//
	// We model the same scenario as a hub edge case: upstream's
	// status is `failed` in hub but downstream is still `active`
	// (e.g. crash between marking upstream and cascading).
	upID := f.seedMission(t, "sub-up-failed", "u", "fails", "failed", nil, 0)
	_ = upID
	// Active downstream gets abandoned on restart regardless of the
	// upstream's failed state — the "abandon all in-flight" policy
	// reaps everything non-terminal. The exact reason on the
	// downstream is "restart: process died" (not the cascade reason)
	// because the active branch fires before the pending-cascade
	// branch.
	downID := f.seedMission(t, "sub-down-orphan", "d", "downstream", "active", []string{upID}, 0)

	exec := f.freshExec(t)
	rep, err := exec.RestoreState(ctx)
	require.NoError(t, err)
	require.NotZero(t, rep.Coordinators)

	snap := exec.Snapshot(ctx, f.coord)
	statuses := map[string]string{}
	for _, n := range snap {
		statuses[n.ID] = n.Status
	}
	assert.Equal(t, graph.StatusFailed, statuses[upID])
	assert.Equal(t, graph.StatusAbandoned, statuses[downID],
		"active downstream is reaped on restart")
}
