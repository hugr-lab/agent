//go:build duckdb_arrow

package store_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hugr-lab/hugen/internal/testenv"
	"github.com/hugr-lab/hugen/pkg/missions/graph"
	"github.com/hugr-lab/hugen/pkg/missions/store"
	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
)

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// newFixture builds a Store + sessstore.Client pair around a fresh
// hub.db. Tests drive both — the fake Dispatcher would in production
// create the session row via sm.Create, here we write straight to hub
// to emulate that shape.
func newFixture(t *testing.T) (*store.Store, *sessstore.Client) {
	t.Helper()
	service, _ := testenv.Engine(t)
	logger := slog.New(slog.NewTextHandler(discardWriter{}, nil))
	testenv.RegisterAgent(t, service, "agt_ag01", "ag01", "test-agent")

	sess, err := sessstore.New(service, sessstore.Options{
		AgentID: "agt_ag01", AgentShort: "ag01", Logger: logger,
	})
	require.NoError(t, err)

	return store.New(sess, service, logger), sess
}

func createCoordinator(t *testing.T, sess *sessstore.Client, id string) {
	t.Helper()
	_, err := sess.CreateSession(context.Background(), sessstore.Record{
		ID: id, AgentID: "agt_ag01", Status: "active", SessionType: sessstore.SessionTypeRoot,
	})
	require.NoError(t, err)
}

// createMission emulates what Dispatcher.RunMission writes when the
// Executor promotes a mission: subagent row with skill / role /
// coord_session_id / depends_on in metadata.
func createMission(
	t *testing.T,
	sess *sessstore.Client,
	id, parent, coord, skill, role, task string,
	dependsOn []string,
) {
	t.Helper()
	meta := map[string]any{
		graph.MetadataKeySkill:        skill,
		graph.MetadataKeyRole:         role,
		graph.MetadataKeyCoordSession: coord,
	}
	if len(dependsOn) > 0 {
		meta[graph.MetadataKeyDependsOn] = dependsOn
	}
	_, err := sess.CreateSession(context.Background(), sessstore.Record{
		ID:              id,
		AgentID:         "agt_ag01",
		Status:          "active",
		SessionType:     sessstore.SessionTypeSubAgent,
		ParentSessionID: parent,
		Mission:         task,
		Metadata:        meta,
	})
	require.NoError(t, err)
}

func TestStore_ListMissions_FiltersSubagentChildren(t *testing.T) {
	st, sess := newFixture(t)
	ctx := context.Background()
	createCoordinator(t, sess, "coord-1")

	createMission(t, sess, "sub-1", "coord-1", "coord-1", "x", "y", "task 1", nil)
	createMission(t, sess, "sub-2", "coord-1", "coord-1", "x", "y", "task 2", []string{"sub-1"})

	ms, err := st.ListMissions(ctx, "coord-1", "")
	require.NoError(t, err)
	require.Len(t, ms, 2)

	byTask := map[string]graph.MissionRecord{}
	for _, m := range ms {
		byTask[m.Task] = m
	}
	assert.Equal(t, "x", byTask["task 1"].Skill)
	assert.Equal(t, "y", byTask["task 1"].Role)
	assert.Equal(t, []string{"sub-1"}, byTask["task 2"].DependsOn)
}

func TestStore_MarkStatus_UpdatesRow(t *testing.T) {
	st, sess := newFixture(t)
	ctx := context.Background()
	createCoordinator(t, sess, "coord-1")
	createMission(t, sess, "sub-1", "coord-1", "coord-1", "x", "y", "task", nil)

	require.NoError(t, st.MarkStatus(ctx, "sub-1", graph.StatusDone))

	ms, err := st.ListMissions(ctx, "coord-1", "")
	require.NoError(t, err)
	require.Len(t, ms, 1)
	assert.Equal(t, graph.StatusDone, ms[0].Status)
}

func TestStore_ListAgentMissions_FlatScan(t *testing.T) {
	st, sess := newFixture(t)
	ctx := context.Background()
	createCoordinator(t, sess, "coord-A")
	createCoordinator(t, sess, "coord-B")

	createMission(t, sess, "a-1", "coord-A", "coord-A", "x", "y", "A1", nil)
	createMission(t, sess, "a-2", "coord-A", "coord-A", "x", "y", "A2", []string{"a-1"})
	createMission(t, sess, "b-1", "coord-B", "coord-B", "x", "y", "B1", nil)
	// Nested spawn: a-1 spawned an a-1-child. parent_session_id = a-1,
	// but coord_session_id still points at coord-A.
	createMission(t, sess, "a-1-child", "a-1", "coord-A", "x", "y", "nested", nil)

	ms, err := st.ListAgentMissions(ctx)
	require.NoError(t, err)
	assert.Len(t, ms, 4)

	byCoord := map[string][]string{}
	for _, m := range ms {
		byCoord[m.CoordSessionID] = append(byCoord[m.CoordSessionID], m.ID)
	}
	assert.ElementsMatch(t, []string{"a-1", "a-2", "a-1-child"}, byCoord["coord-A"])
	assert.ElementsMatch(t, []string{"b-1"}, byCoord["coord-B"])
}

func TestStore_RecordAbandoned_CreatesTerminalRow(t *testing.T) {
	st, sess := newFixture(t)
	ctx := context.Background()
	createCoordinator(t, sess, "coord-1")

	require.NoError(t, st.RecordAbandoned(ctx,
		"sub-abandoned", "coord-1", "coord-1", "x", "y", "never ran",
		[]string{"sub-upstream"}, "upstream failed",
	))

	ms, err := st.ListMissions(ctx, "coord-1", "")
	require.NoError(t, err)
	require.Len(t, ms, 1)
	assert.Equal(t, graph.StatusAbandoned, ms[0].Status)
	assert.Equal(t, []string{"sub-upstream"}, ms[0].DependsOn)
}
