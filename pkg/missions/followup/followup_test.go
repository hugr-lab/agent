//go:build duckdb_arrow

package followup_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/adk/model"
	"google.golang.org/genai"

	agentstore "github.com/hugr-lab/hugen/pkg/agent/store"
	"github.com/hugr-lab/hugen/internal/testenv"
	"github.com/hugr-lab/hugen/pkg/missions/executor"
	"github.com/hugr-lab/hugen/pkg/missions/followup"
	"github.com/hugr-lab/hugen/pkg/missions/graph"
	missionsstore "github.com/hugr-lab/hugen/pkg/missions/store"
	"github.com/hugr-lab/hugen/pkg/models"
	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
)

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// fakeDriver emulates the production dispatcher: creates the session
// row with the same metadata shape then blocks until ctx cancels.
// Tests observe "mission is running" state without needing real LLM.
type fakeDriver struct {
	sess    *sessstore.Client
	started chan graph.DispatchArgs
}

func (f *fakeDriver) RunMission(ctx context.Context, args graph.DispatchArgs) graph.DispatchResult {
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
	f.started <- args
	<-ctx.Done()
	return graph.DispatchResult{Status: graph.StatusAbandoned, Error: "context cancelled"}
}

// --- fixture -----------------------------------------------------

type fixture struct {
	sess   *sessstore.Client
	exec   *executor.Executor
	driver *fakeDriver
	coord  string
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
	_, err = sess.CreateSession(context.Background(), sessstore.Record{
		ID: "coord-fu", AgentID: "agt_ag01", Status: "active",
		SessionType: sessstore.SessionTypeRoot,
	})
	require.NoError(t, err)

	driver := &fakeDriver{sess: sess, started: make(chan graph.DispatchArgs, 16)}
	store := missionsstore.New(sess, service, logger)
	exec := executor.New(executor.Config{
		Store: store, Events: sess, Driver: driver, Logger: logger, Parallelism: 4,
	})
	return &fixture{sess: sess, exec: exec, driver: driver, coord: "coord-fu"}
}

// registerAndStart plans `len(tasks)` independent missions, ticks the
// executor to promote them to running, and drains the driver's
// started-channel so subsequent WaitStarted calls don't race.
func (f *fixture) registerAndStart(t *testing.T, tasks []string) []string {
	t.Helper()
	missions := make([]graph.PlannerMission, 0, len(tasks))
	for i, task := range tasks {
		missions = append(missions, graph.PlannerMission{
			ID: i + 1, Skill: "x", Role: "y", Task: task,
		})
	}
	plan := graph.PlanResult{Missions: missions}
	f.exec.Register(f.coord, &plan)
	f.exec.Tick(context.Background())
	ids := make([]string, 0, len(tasks))
	for range tasks {
		args := <-f.driver.started
		ids = append(ids, args.ChildSessionID)
	}
	return ids
}

// scriptClassifier returns a router whose IntentClassification route
// replays the given raw text as a single LLM response.
func scriptClassifier(content string) (*models.Router, *models.ScriptedLLM) {
	llm := models.NewScriptedLLM("classifier", []models.ScriptedResponse{{Content: content}})
	r := models.NewRouterWithDefault(llm)
	r.SetRoute(models.IntentClassification, llm)
	return r, llm
}

func clfJSON(match any, confidence float64) string {
	payload := map[string]any{"match": match, "confidence": confidence}
	b, _ := json.Marshal(payload)
	return string(b)
}

func mkReq(text string) *model.LLMRequest {
	return &model.LLMRequest{Contents: []*genai.Content{{
		Role: "user", Parts: []*genai.Part{{Text: text}},
	}}}
}

// --- tests -------------------------------------------------------

func TestFollowUp_UnambiguousMatch_Routes(t *testing.T) {
	f := newFixture(t)
	ids := f.registerAndStart(t, []string{"mission one", "mission two"})
	// Address the classifier reply by the real session id (string)
	// rather than the prompt-list index — the prompt order depends on
	// map iteration in promoteRunning + RunningMissions and would
	// otherwise make this test flake against runtime randomisation.
	target := ids[1]

	clfRouter, _ := scriptClassifier(clfJSON(target, 0.9))
	r := followup.New(followup.Config{
		Executor: f.exec, Router: clfRouter, Enabled: true,
	})

	resp, err := r.Decide(context.Background(), f.coord, mkReq("refine mission two"))
	require.NoError(t, err)
	require.NotNil(t, resp, "unambiguous match must short-circuit")
	require.NotEmpty(t, resp.Content.Parts)
	assert.Contains(t, resp.Content.Parts[0].Text, "Relaying")

	// Target session has the new user_message.
	evs, err := f.sess.GetEvents(context.Background(), target)
	require.NoError(t, err)
	var seenRoute bool
	for _, ev := range evs {
		if ev.EventType == sessstore.EventTypeUserMessage && ev.Content == "refine mission two" {
			seenRoute = true
		}
	}
	assert.True(t, seenRoute, "refinement landed on target mission's transcript")

	// Coord got the audit event.
	coordEvs, err := f.sess.GetEvents(context.Background(), f.coord)
	require.NoError(t, err)
	var seenAudit bool
	for _, ev := range coordEvs {
		if ev.EventType == sessstore.EventTypeUserFollowupRouted {
			seenAudit = true
			assert.Equal(t, target, ev.Metadata["target_mission_id"])
		}
	}
	assert.True(t, seenAudit, "coordinator has user_followup_routed audit row")
}

func TestFollowUp_BelowThreshold_Proceeds(t *testing.T) {
	f := newFixture(t)
	f.registerAndStart(t, []string{"mission one"})

	clfRouter, _ := scriptClassifier(clfJSON(1, 0.30))
	r := followup.New(followup.Config{
		Executor: f.exec, Router: clfRouter, Enabled: true,
	})

	resp, err := r.Decide(context.Background(), f.coord, mkReq("refine"))
	require.NoError(t, err)
	assert.Nil(t, resp)
}

func TestFollowUp_WithinTieBand_Proceeds(t *testing.T) {
	f := newFixture(t)
	f.registerAndStart(t, []string{"mission one"})

	// threshold 0.55 + tieBand 0.05 → must exceed 0.60. 0.57 is inside.
	clfRouter, _ := scriptClassifier(clfJSON(1, 0.57))
	r := followup.New(followup.Config{
		Executor: f.exec, Router: clfRouter, Enabled: true,
	})

	resp, err := r.Decide(context.Background(), f.coord, mkReq("refine"))
	require.NoError(t, err)
	assert.Nil(t, resp)
}

func TestFollowUp_NullMatch_Proceeds(t *testing.T) {
	f := newFixture(t)
	f.registerAndStart(t, []string{"mission one"})

	clfRouter, _ := scriptClassifier(clfJSON(nil, 0.0))
	r := followup.New(followup.Config{
		Executor: f.exec, Router: clfRouter, Enabled: true,
	})

	resp, err := r.Decide(context.Background(), f.coord, mkReq("new question"))
	require.NoError(t, err)
	assert.Nil(t, resp)
}

func TestFollowUp_Disabled_ProceedsWithoutClassifierCall(t *testing.T) {
	f := newFixture(t)
	f.registerAndStart(t, []string{"mission one"})

	clfRouter, llm := scriptClassifier(clfJSON(1, 0.99))
	r := followup.New(followup.Config{
		Executor: f.exec, Router: clfRouter, Enabled: false,
	})

	resp, err := r.Decide(context.Background(), f.coord, mkReq("refine"))
	require.NoError(t, err)
	assert.Nil(t, resp)
	assert.Equal(t, 0, llm.Turns(), "disabled → no classifier call")
}

func TestFollowUp_NoRunningMissions_Proceeds(t *testing.T) {
	f := newFixture(t)

	clfRouter, llm := scriptClassifier(clfJSON(1, 0.99))
	r := followup.New(followup.Config{
		Executor: f.exec, Router: clfRouter, Enabled: true,
	})

	resp, err := r.Decide(context.Background(), f.coord, mkReq("refine"))
	require.NoError(t, err)
	assert.Nil(t, resp)
	assert.Equal(t, 0, llm.Turns(), "no running missions → no classifier call")
}

func TestFollowUp_ClassifierParseError_Proceeds(t *testing.T) {
	f := newFixture(t)
	f.registerAndStart(t, []string{"mission one"})

	clfRouter, _ := scriptClassifier("not json at all")
	r := followup.New(followup.Config{
		Executor: f.exec, Router: clfRouter, Enabled: true,
	})

	resp, err := r.Decide(context.Background(), f.coord, mkReq("refine"))
	require.NoError(t, err)
	assert.Nil(t, resp)
}

func TestFollowUp_EmptyUserMessage_Proceeds(t *testing.T) {
	f := newFixture(t)
	f.registerAndStart(t, []string{"mission one"})

	clfRouter, llm := scriptClassifier(clfJSON(1, 0.99))
	r := followup.New(followup.Config{
		Executor: f.exec, Router: clfRouter, Enabled: true,
	})

	resp, err := r.Decide(context.Background(), f.coord, mkReq(""))
	require.NoError(t, err)
	assert.Nil(t, resp)
	assert.Equal(t, 0, llm.Turns(), "empty user message → no classifier call")
}
