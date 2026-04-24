package planner_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hugr-lab/hugen/pkg/missions/graph"
	"github.com/hugr-lab/hugen/pkg/missions/planner"
	"github.com/hugr-lab/hugen/pkg/models"
	"github.com/hugr-lab/hugen/pkg/skills"
)

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func testSkill(name, version string, roles map[string]string) *skills.Skill {
	subs := make(map[string]skills.SubAgentSpec, len(roles))
	for role, desc := range roles {
		subs[role] = skills.SubAgentSpec{Description: desc, Instructions: "noop"}
	}
	return &skills.Skill{Name: name, Version: version, SubAgents: subs}
}

func scriptPlanner(t *testing.T, script string) (*planner.Planner, *models.ScriptedLLM) {
	t.Helper()
	llm := models.NewScriptedLLM("planner-llm", []models.ScriptedResponse{{Content: script}})
	router := models.NewRouterWithDefault(llm)
	logger := slog.New(slog.NewTextHandler(discardWriter{}, nil))
	return planner.New(router, logger, planner.Options{}), llm
}

func TestPlanner_HappyPath(t *testing.T) {
	sk := testSkill("hugr-data", "0.1.0", map[string]string{
		"schema_explorer": "Explore schema",
		"data_analyst":    "Run queries",
	})
	out := models.ScriptedPlannerResponse(
		[]models.ScriptedPlannerMission{
			{ID: 1, Skill: "hugr-data", Role: "schema_explorer", Task: "describe incidents"},
			{ID: 2, Skill: "hugr-data", Role: "schema_explorer", Task: "describe weather"},
			{ID: 3, Skill: "hugr-data", Role: "data_analyst", Task: "join Q1"},
		},
		[]models.ScriptedPlannerEdge{
			{From: 1, To: 3},
			{From: 2, To: 3},
		},
	)
	p, _ := scriptPlanner(t, out)
	plan, err := p.Plan(context.Background(), "coord-1", "build quarterly report",
		[]*skills.Skill{sk}, graph.PlanOptions{})
	require.NoError(t, err)
	assert.Len(t, plan.Missions, 3)
	assert.Len(t, plan.Edges, 2)
	assert.False(t, plan.FromCache)
}

func TestPlanner_ParseError(t *testing.T) {
	sk := testSkill("hugr-data", "0.1.0", map[string]string{"x": "y"})
	p, _ := scriptPlanner(t, "this is not json")
	_, err := p.Plan(context.Background(), "coord-1", "goal", []*skills.Skill{sk}, graph.PlanOptions{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, graph.ErrPlanParse))
}

func TestPlanner_UnknownRole(t *testing.T) {
	sk := testSkill("hugr-data", "0.1.0", map[string]string{"schema_explorer": "desc"})
	out := models.ScriptedPlannerResponse(
		[]models.ScriptedPlannerMission{
			{ID: 1, Skill: "hugr-data", Role: "ghost", Task: "t"},
		},
		nil,
	)
	p, _ := scriptPlanner(t, out)
	_, err := p.Plan(context.Background(), "coord-1", "goal", []*skills.Skill{sk}, graph.PlanOptions{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, graph.ErrUnknownRole))
}

func TestPlanner_CycleRejected(t *testing.T) {
	sk := testSkill("x", "0.1.0", map[string]string{"y": "desc"})
	out := models.ScriptedPlannerResponse(
		[]models.ScriptedPlannerMission{
			{ID: 1, Skill: "x", Role: "y", Task: "a"},
			{ID: 2, Skill: "x", Role: "y", Task: "b"},
		},
		[]models.ScriptedPlannerEdge{
			{From: 1, To: 2},
			{From: 2, To: 1},
		},
	)
	p, _ := scriptPlanner(t, out)
	_, err := p.Plan(context.Background(), "coord-1", "goal", []*skills.Skill{sk}, graph.PlanOptions{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, graph.ErrCyclicGraph))
}

func TestPlanner_EmptyTask(t *testing.T) {
	sk := testSkill("x", "0.1.0", map[string]string{"y": "desc"})
	out := models.ScriptedPlannerResponse(
		[]models.ScriptedPlannerMission{{ID: 1, Skill: "x", Role: "y", Task: ""}},
		nil,
	)
	p, _ := scriptPlanner(t, out)
	_, err := p.Plan(context.Background(), "coord-1", "goal", []*skills.Skill{sk}, graph.PlanOptions{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, graph.ErrEmptyTask))
}

func TestPlanner_CacheHit(t *testing.T) {
	sk := testSkill("x", "0.1.0", map[string]string{"y": "desc"})
	out := models.ScriptedPlannerResponse(
		[]models.ScriptedPlannerMission{{ID: 1, Skill: "x", Role: "y", Task: "t"}},
		nil,
	)
	llm := models.NewScriptedLLM("planner-llm", []models.ScriptedResponse{
		{Content: out}, {Content: out},
	})
	router := models.NewRouterWithDefault(llm)
	p := planner.New(router, slog.New(slog.NewTextHandler(discardWriter{}, nil)), planner.Options{})

	ctx := context.Background()
	first, err := p.Plan(ctx, "coord-1", "goal", []*skills.Skill{sk}, graph.PlanOptions{})
	require.NoError(t, err)
	assert.False(t, first.FromCache)
	assert.Equal(t, 1, llm.Turns())

	second, err := p.Plan(ctx, "coord-1", "goal", []*skills.Skill{sk}, graph.PlanOptions{})
	require.NoError(t, err)
	assert.True(t, second.FromCache)
	assert.Equal(t, 1, llm.Turns(), "cache hit — no second LLM call")
}

func TestPlanner_CacheBypassOnForce(t *testing.T) {
	sk := testSkill("x", "0.1.0", map[string]string{"y": "desc"})
	out := models.ScriptedPlannerResponse(
		[]models.ScriptedPlannerMission{{ID: 1, Skill: "x", Role: "y", Task: "t"}},
		nil,
	)
	llm := models.NewScriptedLLM("planner-llm", []models.ScriptedResponse{
		{Content: out}, {Content: out},
	})
	router := models.NewRouterWithDefault(llm)
	p := planner.New(router, slog.New(slog.NewTextHandler(discardWriter{}, nil)), planner.Options{})

	ctx := context.Background()
	_, err := p.Plan(ctx, "coord-1", "goal", []*skills.Skill{sk}, graph.PlanOptions{})
	require.NoError(t, err)
	second, err := p.Plan(ctx, "coord-1", "goal", []*skills.Skill{sk}, graph.PlanOptions{Force: true})
	require.NoError(t, err)
	assert.False(t, second.FromCache)
	assert.Equal(t, 2, llm.Turns())
}

func TestPlanner_DigestInvalidatesOnSkillLoad(t *testing.T) {
	sk1 := testSkill("x", "0.1.0", map[string]string{"y": "desc"})
	sk2 := testSkill("z", "0.1.0", map[string]string{"w": "other"})
	out := models.ScriptedPlannerResponse(
		[]models.ScriptedPlannerMission{{ID: 1, Skill: "x", Role: "y", Task: "t"}},
		nil,
	)
	out2 := models.ScriptedPlannerResponse(
		[]models.ScriptedPlannerMission{{ID: 1, Skill: "z", Role: "w", Task: "t2"}},
		nil,
	)
	llm := models.NewScriptedLLM("planner-llm", []models.ScriptedResponse{
		{Content: out}, {Content: out2},
	})
	router := models.NewRouterWithDefault(llm)
	p := planner.New(router, slog.New(slog.NewTextHandler(discardWriter{}, nil)), planner.Options{})

	ctx := context.Background()
	_, err := p.Plan(ctx, "coord-1", "goal", []*skills.Skill{sk1}, graph.PlanOptions{})
	require.NoError(t, err)
	_, err = p.Plan(ctx, "coord-1", "goal", []*skills.Skill{sk1, sk2}, graph.PlanOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, llm.Turns(), "skills digest changed → cache miss → new call")
}

func TestPlanner_SessionCloseDropsCache(t *testing.T) {
	sk := testSkill("x", "0.1.0", map[string]string{"y": "desc"})
	out := models.ScriptedPlannerResponse(
		[]models.ScriptedPlannerMission{{ID: 1, Skill: "x", Role: "y", Task: "t"}},
		nil,
	)
	llm := models.NewScriptedLLM("planner-llm", []models.ScriptedResponse{
		{Content: out}, {Content: out},
	})
	router := models.NewRouterWithDefault(llm)
	p := planner.New(router, slog.New(slog.NewTextHandler(discardWriter{}, nil)), planner.Options{})

	ctx := context.Background()
	_, err := p.Plan(ctx, "coord-1", "goal", []*skills.Skill{sk}, graph.PlanOptions{})
	require.NoError(t, err)
	p.OnCoordinatorClose("coord-1")
	_, err = p.Plan(ctx, "coord-1", "goal", []*skills.Skill{sk}, graph.PlanOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, llm.Turns(), "session close drops cache")
}

func TestPlanner_CrossSessionIsolation(t *testing.T) {
	sk := testSkill("x", "0.1.0", map[string]string{"y": "desc"})
	out := models.ScriptedPlannerResponse(
		[]models.ScriptedPlannerMission{{ID: 1, Skill: "x", Role: "y", Task: "t"}},
		nil,
	)
	llm := models.NewScriptedLLM("planner-llm", []models.ScriptedResponse{
		{Content: out}, {Content: out},
	})
	router := models.NewRouterWithDefault(llm)
	p := planner.New(router, slog.New(slog.NewTextHandler(discardWriter{}, nil)), planner.Options{})

	ctx := context.Background()
	_, err := p.Plan(ctx, "coord-1", "goal", []*skills.Skill{sk}, graph.PlanOptions{})
	require.NoError(t, err)
	_, err = p.Plan(ctx, "coord-2", "goal", []*skills.Skill{sk}, graph.PlanOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, llm.Turns(), "different coordinator → independent cache")
}

func TestPlanner_ConcurrentSameKey_SingleCall(t *testing.T) {
	sk := testSkill("x", "0.1.0", map[string]string{"y": "desc"})
	out := models.ScriptedPlannerResponse(
		[]models.ScriptedPlannerMission{{ID: 1, Skill: "x", Role: "y", Task: "t"}},
		nil,
	)
	llm := models.NewScriptedLLM("planner-llm", []models.ScriptedResponse{
		{Content: out},
	})
	router := models.NewRouterWithDefault(llm)
	p := planner.New(router, slog.New(slog.NewTextHandler(discardWriter{}, nil)), planner.Options{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := p.Plan(ctx, "coord-1", "goal", []*skills.Skill{sk}, graph.PlanOptions{})
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, 1, llm.Turns(), "single-flight collapsed 4 concurrent calls into 1")
}
