package graph_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hugr-lab/hugen/pkg/missions/graph"
)

// TestValidatePlan exercises the structural invariants enforced
// before a PlanResult is persisted or seeded into the DAG.
func TestValidatePlan(t *testing.T) {
	t.Run("rejects empty missions", func(t *testing.T) {
		err := graph.ValidatePlan(graph.PlanResult{})
		require.Error(t, err)
		assert.True(t, errors.Is(err, graph.ErrNoMissions))
	})

	t.Run("rejects duplicate ids", func(t *testing.T) {
		err := graph.ValidatePlan(graph.PlanResult{
			Missions: []graph.PlannerMission{
				{ID: 1, Skill: "x", Role: "y", Task: "a"},
				{ID: 1, Skill: "x", Role: "y", Task: "b"},
			},
		})
		require.Error(t, err)
		assert.True(t, errors.Is(err, graph.ErrDuplicateNode))
	})

	t.Run("rejects empty task", func(t *testing.T) {
		err := graph.ValidatePlan(graph.PlanResult{
			Missions: []graph.PlannerMission{{ID: 1, Skill: "x", Role: "y", Task: ""}},
		})
		require.Error(t, err)
		assert.True(t, errors.Is(err, graph.ErrEmptyTask))
	})

	t.Run("rejects self-loop edges", func(t *testing.T) {
		err := graph.ValidatePlan(graph.PlanResult{
			Missions: []graph.PlannerMission{{ID: 1, Skill: "x", Role: "y", Task: "t"}},
			Edges:    []graph.PlannerEdge{{From: 1, To: 1}},
		})
		require.Error(t, err)
		assert.True(t, errors.Is(err, graph.ErrCyclicGraph))
	})

	t.Run("rejects unknown edge nodes", func(t *testing.T) {
		err := graph.ValidatePlan(graph.PlanResult{
			Missions: []graph.PlannerMission{{ID: 1, Skill: "x", Role: "y", Task: "t"}},
			Edges:    []graph.PlannerEdge{{From: 1, To: 99}},
		})
		require.Error(t, err)
		assert.True(t, errors.Is(err, graph.ErrUnknownEdgeNode))
	})

	t.Run("rejects cyclic graph", func(t *testing.T) {
		err := graph.ValidatePlan(graph.PlanResult{
			Missions: []graph.PlannerMission{
				{ID: 1, Skill: "x", Role: "y", Task: "a"},
				{ID: 2, Skill: "x", Role: "y", Task: "b"},
				{ID: 3, Skill: "x", Role: "y", Task: "c"},
			},
			Edges: []graph.PlannerEdge{
				{From: 1, To: 2}, {From: 2, To: 3}, {From: 3, To: 1},
			},
		})
		require.Error(t, err)
		assert.True(t, errors.Is(err, graph.ErrCyclicGraph))
	})

	t.Run("accepts a valid DAG", func(t *testing.T) {
		err := graph.ValidatePlan(graph.PlanResult{
			Missions: []graph.PlannerMission{
				{ID: 1, Skill: "x", Role: "y", Task: "a"},
				{ID: 2, Skill: "x", Role: "y", Task: "b"},
				{ID: 3, Skill: "x", Role: "y", Task: "c"},
			},
			Edges: []graph.PlannerEdge{
				{From: 1, To: 3}, {From: 2, To: 3},
			},
		})
		require.NoError(t, err)
	})
}
