//go:build duckdb_arrow

package missions_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	adksession "google.golang.org/adk/session"

	agentstore "github.com/hugr-lab/hugen/pkg/agent/store"
	"github.com/hugr-lab/hugen/internal/testenv"
	"github.com/hugr-lab/hugen/pkg/missions"
	"github.com/hugr-lab/hugen/pkg/missions/executor"
	"github.com/hugr-lab/hugen/pkg/missions/graph"
	missionsstore "github.com/hugr-lab/hugen/pkg/missions/store"
	"github.com/hugr-lab/hugen/pkg/sessions"
	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
	"github.com/hugr-lab/hugen/pkg/skills"
	"github.com/hugr-lab/hugen/pkg/tools"
)

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// stubSkillsManager satisfies skills.Manager with a fixed catalog
// of in-memory skills. Only Load is exercised by the spawn path;
// the other methods return empty results so the session manager's
// autoload sweep is a no-op.
type stubSkillsManager struct {
	mu      sync.Mutex
	catalog map[string]*skills.Skill
}

func (s *stubSkillsManager) Load(_ context.Context, name string) (*skills.Skill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sk, ok := s.catalog[name]; ok {
		return sk, nil
	}
	return nil, errSkillNotFound
}

func (s *stubSkillsManager) List(context.Context) ([]skills.SkillMeta, error) {
	return nil, nil
}
func (s *stubSkillsManager) Reference(context.Context, string, string) (string, error) {
	return "", nil
}
func (s *stubSkillsManager) RenderCatalog([]skills.SkillMeta) string { return "" }
func (s *stubSkillsManager) AutoloadNames(context.Context) ([]string, error) {
	return nil, nil
}
func (s *stubSkillsManager) AutoloadNamesFor(context.Context, string) ([]string, error) {
	return nil, nil
}

var errSkillNotFound = errSkillsNotFound{}

type errSkillsNotFound struct{}

func (errSkillsNotFound) Error() string { return "skill not found" }

// fixture wires Executor + spawn service + a stub skills manager +
// a real sessions.Manager (with the test hub.db).
type fixture struct {
	exec  *executor.Executor
	mgr   *sessions.Manager
	svc   *missions.SpawnService
	skMgr *stubSkillsManager
	coord string
}

func newFixture(t *testing.T) *fixture {
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

	store := missionsstore.New(sess, service, logger)
	exec := executor.New(executor.Config{
		Store: store, Events: sess, Driver: nil, Logger: logger, Parallelism: 4,
	})

	skMgr := &stubSkillsManager{catalog: map[string]*skills.Skill{
		"domain": {
			Name:    "domain",
			Version: "1.0.0",
			SubAgents: map[string]skills.SubAgentSpec{
				"scout":  {Description: "scout", CanSpawn: true, MaxDepth: 2},
				"plain":  {Description: "plain"},
				"deeper": {Description: "deeper", CanSpawn: true, MaxDepth: 5},
			},
		},
	}}

	mgr, err := sessions.New(sessions.Config{
		Skills:       skMgr,
		Tools:        tools.New(logger),
		Sessions:     sess,
		Logger:       logger,
		Constitution: "TEST",
	})
	require.NoError(t, err)

	svc := missions.NewSpawnService(missions.SpawnConfig{
		Executor:      exec,
		Sessions:      mgr,
		Skills:        skMgr,
		MaxSpawnDepth: 4,
	})

	coord := "coord-spawn"
	_, err = mgr.Create(context.Background(), &adksession.CreateRequest{
		AppName: "test-app", UserID: "u", SessionID: coord,
	})
	require.NoError(t, err)
	return &fixture{exec: exec, mgr: mgr, svc: svc, skMgr: skMgr, coord: coord}
}

// makeSubAgentSession seeds a sub-agent session with the (skill, role)
// linkage state keys that sessions.Manager.Create absorbs into the
// session's metadata + the hub Record. parentID == "" defaults to
// the coord id; pass another sub-agent id to extend the chain.
func (f *fixture) makeSubAgentSession(t *testing.T, id, role, parentID string) {
	t.Helper()
	if parentID == "" {
		parentID = f.coord
	}
	state := map[string]any{
		"__session_type__":      "subagent",
		"__parent_session_id__": parentID,
		"__skill__":             "domain",
		"__role__":              role,
		"__coord_session_id__":  f.coord,
		"__mission__":           "task for " + role,
	}
	_, err := f.mgr.Create(context.Background(), &adksession.CreateRequest{
		AppName:   "test-app",
		UserID:    "u",
		SessionID: id,
		State:     state,
	})
	require.NoError(t, err)
}

// ---- Tests -------------------------------------------------------

func TestSpawn_UnauthorisedRole(t *testing.T) {
	f := newFixture(t)
	f.makeSubAgentSession(t, "sub-plain", "plain", "")

	before := len(f.exec.Snapshot(context.Background(), f.coord))
	out := f.svc.Spawn(context.Background(), "sub-plain", map[string]any{
		"skill": "domain", "role": "scout", "task": "go look",
	})
	require.Contains(t, out, "error")
	assert.Contains(t, out["error"], "is not authorised")
	assert.NotContains(t, out, "mission_id", "refused spawn returns no mission id")

	after := len(f.exec.Snapshot(context.Background(), f.coord))
	assert.Equal(t, before, after, "refused spawn must not register a new mission")
}

func TestSpawn_DepthExceeded(t *testing.T) {
	f := newFixture(t)
	// Chain: coord → sub-1 → sub-2 → sub-3. With scout's max_depth=2,
	// depth(sub-3)=3, depth+1 = 4 > 2 → refused.
	f.makeSubAgentSession(t, "sub-1", "scout", "")
	f.makeSubAgentSession(t, "sub-2", "scout", "sub-1")
	f.makeSubAgentSession(t, "sub-3", "scout", "sub-2")

	out := f.svc.Spawn(context.Background(), "sub-3", map[string]any{
		"skill": "domain", "role": "scout", "task": "deep dive",
	})
	require.Contains(t, out, "error")
	assert.Contains(t, out["error"], "spawn depth limit reached")
}

func TestSpawn_HappyPath(t *testing.T) {
	f := newFixture(t)
	f.makeSubAgentSession(t, "sub-scout", "scout", "")

	out := f.svc.Spawn(context.Background(), "sub-scout", map[string]any{
		"skill": "domain", "role": "scout", "task": "deeper investigation",
	})
	require.NotContains(t, out, "error", "happy path returns no error envelope: %v", out)
	missionID, _ := out["mission_id"].(string)
	require.NotEmpty(t, missionID, "spawn returned a mission id")
	assert.Equal(t, graph.StatusPending, out["status"])

	// Mission landed in the coordinator's DAG (peer of caller).
	snap := f.exec.Snapshot(context.Background(), f.coord)
	require.Len(t, snap, 1)
	assert.Equal(t, missionID, snap[0].ID)
	assert.Equal(t, "scout", snap[0].Role)
	assert.Equal(t, "deeper investigation", snap[0].Task)
}

func TestSpawn_UnknownTargetRole(t *testing.T) {
	f := newFixture(t)
	f.makeSubAgentSession(t, "sub-scout", "scout", "")

	out := f.svc.Spawn(context.Background(), "sub-scout", map[string]any{
		"skill": "domain", "role": "ghost", "task": "boo",
	})
	require.Contains(t, out, "error")
	assert.Contains(t, out["error"], "unknown (skill, role)")
}

func TestSpawn_DependsOnAcceptsExistingMission(t *testing.T) {
	f := newFixture(t)
	f.makeSubAgentSession(t, "sub-scout", "scout", "")

	plan := graph.PlanResult{Missions: []graph.PlannerMission{
		{ID: 1, Skill: "domain", Role: "plain", Task: "peer"},
	}}
	f.exec.Register(f.coord, &plan)
	peer := plan.ChildIDs[1]
	require.NotEmpty(t, peer)

	out := f.svc.Spawn(context.Background(), "sub-scout", map[string]any{
		"skill": "domain", "role": "scout", "task": "depends on peer",
		"depends_on": []any{peer},
	})
	require.NotContains(t, out, "error", "valid depends_on must succeed: %v", out)
	require.NotEmpty(t, out["mission_id"])

	require.Len(t, f.exec.Snapshot(context.Background(), f.coord), 2)
}

func TestSpawn_RejectsCrossGraphDep(t *testing.T) {
	f := newFixture(t)
	f.makeSubAgentSession(t, "sub-scout", "scout", "")

	out := f.svc.Spawn(context.Background(), "sub-scout", map[string]any{
		"skill": "domain", "role": "scout", "task": "outsider dep",
		"depends_on": []any{"sub_does-not-exist"},
	})
	require.Contains(t, out, "error")
	assert.Contains(t, out["error"], "not found in coordinator")
}

func TestSpawn_NotASubAgentSession(t *testing.T) {
	f := newFixture(t)
	// Coordinator session has no role state — spawn must refuse.
	out := f.svc.Spawn(context.Background(), f.coord, map[string]any{
		"skill": "domain", "role": "scout", "task": "from coord",
	})
	require.Contains(t, out, "error")
	assert.Contains(t, out["error"], "only available on sub-agent sessions")
}
