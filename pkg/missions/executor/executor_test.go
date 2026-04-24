//go:build duckdb_arrow

package executor_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentstore "github.com/hugr-lab/hugen/pkg/agent/store"
	"github.com/hugr-lab/hugen/internal/testenv"
	"github.com/hugr-lab/hugen/pkg/missions/executor"
	"github.com/hugr-lab/hugen/pkg/missions/graph"
	missionsstore "github.com/hugr-lab/hugen/pkg/missions/store"
	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
)

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// fakeDriver is a deterministic MissionDriver built on channels. Each
// RunMission call publishes its DispatchArgs on `started` and BLOCKS
// on a per-mission completion channel until the test pushes a result
// via Release / ReleaseAll. No time.Sleep anywhere.
//
// Emulates what the production Dispatcher does on mission start:
// create the sub-agent session row in hub.db with skill / role /
// coord_session_id / depends_on in metadata.
type fakeDriver struct {
	started chan graph.DispatchArgs
	sess    *sessstore.Client

	mu          sync.Mutex
	completions map[string]chan graph.DispatchResult
}

func newFakeDriver(sess *sessstore.Client) *fakeDriver {
	return &fakeDriver{
		started:     make(chan graph.DispatchArgs, 64),
		sess:        sess,
		completions: map[string]chan graph.DispatchResult{},
	}
}

func (f *fakeDriver) RunMission(ctx context.Context, args graph.DispatchArgs) graph.DispatchResult {
	// Emulate production Dispatcher: create the session row up front.
	meta := map[string]any{
		graph.MetadataKeySkill:        args.Skill,
		graph.MetadataKeyRole:         args.Role,
		graph.MetadataKeyCoordSession: args.CoordSessionID,
	}
	if len(args.DependsOn) > 0 {
		meta[graph.MetadataKeyDependsOn] = args.DependsOn
	}
	_, _ = f.sess.CreateSession(ctx, sessstore.Record{
		ID:              args.ChildSessionID,
		Status:          "active",
		SessionType:     sessstore.SessionTypeSubAgent,
		ParentSessionID: args.ParentSessionID,
		Mission:         args.Task,
		Metadata:        meta,
	})

	f.mu.Lock()
	ch, ok := f.completions[args.ChildSessionID]
	if !ok {
		ch = make(chan graph.DispatchResult, 1)
		f.completions[args.ChildSessionID] = ch
	}
	f.mu.Unlock()

	f.started <- args

	select {
	case res := <-ch:
		return res
	case <-ctx.Done():
		return graph.DispatchResult{Status: graph.StatusAbandoned, Error: "context cancelled"}
	}
}

// WaitStarted blocks until `n` missions have called RunMission.
func (f *fakeDriver) WaitStarted(t *testing.T, n int) []graph.DispatchArgs {
	t.Helper()
	out := make([]graph.DispatchArgs, 0, n)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for len(out) < n {
		select {
		case a := <-f.started:
			out = append(out, a)
		case <-timer.C:
			t.Fatalf("timed out waiting for %d missions to start (got %d)", n, len(out))
		}
	}
	return out
}

// AssertNoMoreStarts expects no further RunMission calls within a
// short window. Used for "respects parallelism cap" / "serialises
// overlapping ticks" checks — negative assertions must have a window.
func (f *fakeDriver) AssertNoMoreStarts(t *testing.T, window time.Duration) {
	t.Helper()
	timer := time.NewTimer(window)
	defer timer.Stop()
	select {
	case a := <-f.started:
		t.Fatalf("unexpected additional mission start: %+v", a)
	case <-timer.C:
		return
	}
}

func (f *fakeDriver) Release(missionID string, res graph.DispatchResult) {
	f.mu.Lock()
	ch, ok := f.completions[missionID]
	if !ok {
		ch = make(chan graph.DispatchResult, 1)
		f.completions[missionID] = ch
	}
	f.mu.Unlock()
	ch <- res
}

// fixture bundles hub.db + Store + Executor + fake driver.
type fixture struct {
	store    *missionsstore.Store
	sess     *sessstore.Client
	exec     *executor.Executor
	driver   *fakeDriver
	coord    string
	reported chan string
}

func newFixture(t *testing.T, parallelism int) *fixture {
	t.Helper()
	service, _ := testenv.Engine(t)
	logger := slog.New(slog.NewTextHandler(discardWriter{}, nil))

	reg, err := agentstore.New(service, agentstore.Options{
		AgentID: "agt_ag01", AgentShort: "ag01", Logger: logger,
	})
	require.NoError(t, err)
	require.NoError(t, reg.RegisterAgent(context.Background(), agentstore.Agent{
		ID: "agt_ag01", AgentTypeID: "hugr-data", ShortID: "ag01",
		Name: "test-agent", Status: "active",
	}))

	sess, err := sessstore.New(service, sessstore.Options{
		AgentID: "agt_ag01", AgentShort: "ag01", Logger: logger,
	})
	require.NoError(t, err)

	_, err = sess.CreateSession(context.Background(), sessstore.Record{
		ID: "coord-exec", AgentID: "agt_ag01", Status: "active",
		SessionType: sessstore.SessionTypeRoot,
	})
	require.NoError(t, err)

	store := missionsstore.New(sess, service, logger)
	driver := newFakeDriver(sess)
	reported := make(chan string, 64)

	exec := executor.New(executor.Config{
		Store:       store,
		Events:      sess,
		Driver:      driver,
		Logger:      logger,
		Parallelism: parallelism,
	})
	exec.OnMissionReported = func(id string) { reported <- id }

	return &fixture{
		store:    store,
		sess:     sess,
		exec:     exec,
		driver:   driver,
		coord:    "coord-exec",
		reported: reported,
	}
}

func (f *fixture) insertPlan(_ *testing.T, plan graph.PlanResult) graph.PlanResult {
	f.exec.Register(f.coord, &plan)
	return plan
}

// waitReported blocks until n dispatcher goroutines have handed off
// their results to the executor's internal channel. After this
// returns, the next Tick is guaranteed to drain those terminals.
func (f *fixture) waitReported(t *testing.T, n int) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for i := 0; i < n; i++ {
		select {
		case <-f.reported:
		case <-timer.C:
			t.Fatalf("timed out waiting for %d mission reports (got %d)", n, i)
		}
	}
}

func (f *fixture) releaseAndTick(t *testing.T, missionID string, res graph.DispatchResult) {
	t.Helper()
	f.driver.Release(missionID, res)
	f.waitReported(t, 1)
	f.exec.Tick(context.Background())
}

// ---- Tests -------------------------------------------------------

func TestExecutor_Tick_PromotesReadyWithinCap(t *testing.T) {
	f := newFixture(t, 3)
	plan := graph.PlanResult{
		Missions: []graph.PlannerMission{
			{ID: 1, Skill: "x", Role: "y", Task: "t1"},
			{ID: 2, Skill: "x", Role: "y", Task: "t2"},
			{ID: 3, Skill: "x", Role: "y", Task: "t3"},
			{ID: 4, Skill: "x", Role: "y", Task: "t4"},
			{ID: 5, Skill: "x", Role: "y", Task: "t5"},
		},
	}
	f.insertPlan(t, plan)

	f.exec.Tick(context.Background())

	started := f.driver.WaitStarted(t, 3)
	assert.Len(t, started, 3)
	f.driver.AssertNoMoreStarts(t, 50*time.Millisecond)

	events, err := f.sess.GetEvents(context.Background(), f.coord)
	require.NoError(t, err)
	spawnCount := 0
	for _, ev := range events {
		if ev.EventType == sessstore.EventTypeAgentSpawn {
			spawnCount++
		}
	}
	assert.Equal(t, 3, spawnCount)
}

func TestExecutor_Tick_SerialisesCycles(t *testing.T) {
	f := newFixture(t, 4)
	plan := graph.PlanResult{
		Missions: []graph.PlannerMission{
			{ID: 1, Skill: "x", Role: "y", Task: "t1"},
			{ID: 2, Skill: "x", Role: "y", Task: "t2"},
		},
	}
	f.insertPlan(t, plan)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.exec.Tick(context.Background())
		}()
	}
	wg.Wait()
	started := f.driver.WaitStarted(t, 2)
	assert.Len(t, started, 2)
	f.driver.AssertNoMoreStarts(t, 50*time.Millisecond)
}

func TestExecutor_Tick_PromotesReadyAfterDepDone(t *testing.T) {
	f := newFixture(t, 4)
	plan := graph.PlanResult{
		Missions: []graph.PlannerMission{
			{ID: 1, Skill: "x", Role: "y", Task: "upstream"},
			{ID: 2, Skill: "x", Role: "y", Task: "downstream"},
		},
		Edges: []graph.PlannerEdge{{From: 1, To: 2}},
	}
	plan = f.insertPlan(t, plan)

	ctx := context.Background()
	f.exec.Tick(ctx)
	started := f.driver.WaitStarted(t, 1)
	require.Len(t, started, 1)
	assert.Equal(t, plan.ChildIDs[1], started[0].ChildSessionID)
	f.driver.AssertNoMoreStarts(t, 50*time.Millisecond)

	f.releaseAndTick(t, plan.ChildIDs[1], graph.DispatchResult{
		Status: graph.StatusDone, Summary: "s", TurnsUsed: 1,
	})

	started2 := f.driver.WaitStarted(t, 1)
	require.Len(t, started2, 1)
	assert.Equal(t, plan.ChildIDs[2], started2[0].ChildSessionID)

	f.releaseAndTick(t, plan.ChildIDs[2], graph.DispatchResult{
		Status: graph.StatusDone, Summary: "s", TurnsUsed: 1,
	})
}

func TestExecutor_Tick_PropagatesAbandonOnFailure(t *testing.T) {
	f := newFixture(t, 4)
	plan := graph.PlanResult{
		Missions: []graph.PlannerMission{
			{ID: 1, Skill: "x", Role: "a", Task: "fails"},
			{ID: 2, Skill: "x", Role: "b", Task: "downstream"},
		},
		Edges: []graph.PlannerEdge{{From: 1, To: 2}},
	}
	plan = f.insertPlan(t, plan)

	ctx := context.Background()
	f.exec.Tick(ctx)
	started := f.driver.WaitStarted(t, 1)
	require.Equal(t, plan.ChildIDs[1], started[0].ChildSessionID)

	f.releaseAndTick(t, plan.ChildIDs[1], graph.DispatchResult{
		Status: graph.StatusFailed, Error: "boom", TurnsUsed: 1,
	})

	f.driver.AssertNoMoreStarts(t, 100*time.Millisecond)

	ms, err := f.store.ListMissions(ctx, f.coord, "")
	require.NoError(t, err)
	byTask := map[string]graph.MissionRecord{}
	for _, m := range ms {
		byTask[m.Task] = m
	}
	assert.Equal(t, graph.StatusFailed, byTask["fails"].Status)
	assert.Equal(t, graph.StatusAbandoned, byTask["downstream"].Status)

	events, err := f.sess.GetEvents(ctx, f.coord)
	require.NoError(t, err)
	resultCount := 0
	for _, ev := range events {
		if ev.EventType == sessstore.EventTypeAgentResult {
			resultCount++
		}
	}
	assert.Equal(t, 2, resultCount)
}

func TestExecutor_EmitsSpawnAndResultOncePerMission(t *testing.T) {
	f := newFixture(t, 4)
	plan := graph.PlanResult{
		Missions: []graph.PlannerMission{{ID: 1, Skill: "x", Role: "y", Task: "t"}},
	}
	plan = f.insertPlan(t, plan)

	ctx := context.Background()
	f.exec.Tick(ctx)
	f.driver.WaitStarted(t, 1)
	f.releaseAndTick(t, plan.ChildIDs[1], graph.DispatchResult{
		Status: graph.StatusDone, Summary: "done", TurnsUsed: 3,
	})

	events, err := f.sess.GetEvents(ctx, f.coord)
	require.NoError(t, err)
	spawns, results := 0, 0
	for _, ev := range events {
		switch ev.EventType {
		case sessstore.EventTypeAgentSpawn:
			spawns++
		case sessstore.EventTypeAgentResult:
			results++
		}
	}
	assert.Equal(t, 1, spawns)
	assert.Equal(t, 1, results)
}

func TestExecutor_EmitsAbstainedHeuristic(t *testing.T) {
	f := newFixture(t, 4)
	plan := graph.PlanResult{
		Missions: []graph.PlannerMission{{ID: 1, Skill: "x", Role: "y", Task: "t"}},
	}
	plan = f.insertPlan(t, plan)

	ctx := context.Background()
	f.exec.Tick(ctx)
	f.driver.WaitStarted(t, 1)
	f.releaseAndTick(t, plan.ChildIDs[1], graph.DispatchResult{
		Status:       graph.StatusDone,
		Summary:      "I can't help with this.",
		Abstained:    true,
		AbstainedWhy: "out of scope",
		TurnsUsed:    1,
	})

	events, err := f.sess.GetEvents(ctx, f.coord)
	require.NoError(t, err)
	var seenResult, seenAbstain bool
	for _, ev := range events {
		switch ev.EventType {
		case sessstore.EventTypeAgentResult:
			seenResult = true
		case sessstore.EventTypeAgentAbstained:
			seenAbstain = true
		}
	}
	assert.True(t, seenResult)
	assert.True(t, seenAbstain)
}

func TestExecutor_ResultSummaryEmbedded(t *testing.T) {
	f := newFixture(t, 4)
	plan := graph.PlanResult{
		Missions: []graph.PlannerMission{{ID: 1, Skill: "x", Role: "y", Task: "t"}},
	}
	plan = f.insertPlan(t, plan)

	ctx := context.Background()
	f.exec.Tick(ctx)
	f.driver.WaitStarted(t, 1)
	f.releaseAndTick(t, plan.ChildIDs[1], graph.DispatchResult{
		Status:    graph.StatusDone,
		Summary:   "meaningful content worth embedding",
		TurnsUsed: 1,
	})

	events, err := f.sess.GetEvents(ctx, f.coord)
	require.NoError(t, err)
	for _, ev := range events {
		if ev.EventType == sessstore.EventTypeAgentResult {
			got, _ := ev.Metadata["summary"].(string)
			assert.Equal(t, "meaningful content worth embedding", got)
			return
		}
	}
	t.Fatal("no agent_result event seen")
}

func TestExecutor_ParallelMissionsConcurrent(t *testing.T) {
	f := newFixture(t, 4)
	plan := graph.PlanResult{
		Missions: []graph.PlannerMission{
			{ID: 1, Skill: "x", Role: "y", Task: "t1"},
			{ID: 2, Skill: "x", Role: "y", Task: "t2"},
			{ID: 3, Skill: "x", Role: "y", Task: "t3"},
		},
	}
	plan = f.insertPlan(t, plan)

	ctx := context.Background()
	f.exec.Tick(ctx)

	started := f.driver.WaitStarted(t, 3)
	assert.Len(t, started, 3)

	for _, a := range started {
		f.driver.Release(a.ChildSessionID, graph.DispatchResult{
			Status: graph.StatusDone, Summary: "s", TurnsUsed: 1,
		})
	}
	f.waitReported(t, len(started))
	f.exec.Tick(context.Background())

	events, err := f.sess.GetEvents(ctx, f.coord)
	require.NoError(t, err)
	resultCount := 0
	for _, ev := range events {
		if ev.EventType == sessstore.EventTypeAgentResult {
			resultCount++
		}
	}
	assert.Equal(t, 3, resultCount)
}
