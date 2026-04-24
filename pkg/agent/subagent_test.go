package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	adksession "google.golang.org/adk/session"

	"github.com/hugr-lab/hugen/pkg/models"
	"github.com/hugr-lab/hugen/pkg/sessions"
	"github.com/hugr-lab/hugen/pkg/skills"
	"github.com/hugr-lab/hugen/pkg/tools"
)

// makeDispatchSkillsDir scaffolds a single skill `domain` with one
// provider (`hugr`) and one specialist role (`schema_explorer`).
// Mirrors what skills/hugr-data/SKILL.md will look like once the
// production frontmatter edit ships in T106.
func makeDispatchSkillsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	domainDir := filepath.Join(dir, "domain")
	require.NoError(t, os.MkdirAll(domainDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(domainDir, "SKILL.md"), []byte(`---
name: domain
version: "0.1.0"
description: Domain skill for tests
providers:
  - name: hugr
    provider: hugr-main
sub_agents:
  schema_explorer:
    description: Discovers schemas of one module.
    intent: tool_calling
    tools: [hugr]
    max_turns: 5
    summary_max_tokens: 200
    instructions: |
      You are a schema explorer for tests.
      Return a one-line summary, no tool calls.
---

Skill body — usually included in the prompt downstream.`), 0o644))

	return dir
}

// dispatchHarness wires Dispatcher to a SessionManager + scripted
// LLMs. The "coordinator" model isn't actually invoked in these
// tests (we call Dispatcher.Run directly, bypassing the coordinator
// turn loop) — what matters is the specialist model and the child
// session lifecycle.
type dispatchHarness struct {
	manager       *sessions.Manager
	skills        skills.Manager
	dispatcher    *Dispatcher
	specialistLLM *models.ScriptedLLM
	skillsRoot    string
}

func newDispatchHarness(t *testing.T, specialistScript []models.ScriptedResponse) *dispatchHarness {
	t.Helper()

	skillsRoot := makeDispatchSkillsDir(t)
	skMgr, err := skills.NewFileManager(skillsRoot)
	require.NoError(t, err)

	tm := tools.New(nil)
	tm.AddProvider(tools.FakeProvider{N: "hugr-main", T: tools.FakeTools("schema_lookup")})

	sm, err := sessions.New(sessions.Config{
		Skills:       skMgr,
		Tools:        tm,
		Constitution: "TEST",
	})
	require.NoError(t, err)

	specialistLLM := models.NewScriptedLLM("test-cheap-model", specialistScript)
	router := models.NewRouterWithDefault(models.NewScriptedLLM("test-strong-model", nil))
	router.SetRoute(models.IntentToolCalling, specialistLLM)
	// Set generous budget so pre-flight refusal doesn't fire.
	router.SetBudgets(map[string]int{
		"test-strong-model": 1_000_000,
		"test-cheap-model":  100_000,
	}, 0)

	disp, err := NewDispatcher(DispatcherConfig{
		Sessions: sm,
		Skills:   skMgr,
		Router:   router,
		Timeout:  10 * time.Second,
	})
	require.NoError(t, err)

	return &dispatchHarness{
		manager:       sm,
		skills:        skMgr,
		dispatcher:    disp,
		specialistLLM: specialistLLM,
		skillsRoot:    skillsRoot,
	}
}

// makeCoordinator opens a coordinator (root) session in the harness
// — the parent for sub-agent dispatches.
func (h *dispatchHarness) makeCoordinator(t *testing.T, id string) {
	t.Helper()
	_, err := h.manager.Create(context.Background(), &adksession.CreateRequest{
		AppName:   "test-app",
		UserID:    "test-user",
		SessionID: id,
	})
	require.NoError(t, err)
}

// loadSpec loads the skill from disk and returns its
// schema_explorer SubAgentSpec — exactly the path the production
// dispatcher will use to pick the spec at tool-invocation time.
func (h *dispatchHarness) loadSpec(t *testing.T) skills.SubAgentSpec {
	t.Helper()
	sk, err := h.skills.Load(context.Background(), "domain")
	require.NoError(t, err)
	spec, ok := sk.SubAgents["schema_explorer"]
	require.True(t, ok, "schema_explorer role must be parsed")
	return spec
}

// ----- T110: happy path -----

func TestDispatcher_Run_HappyPath(t *testing.T) {
	// Specialist replies with a single text turn (no tools), so the
	// dispatcher captures it as the summary, marks the child completed,
	// and returns.
	h := newDispatchHarness(t, []models.ScriptedResponse{
		{Content: "tf.incidents has 14 fields, severity is enum (1-3), spatial coords SRID 4326."},
	})
	h.makeCoordinator(t, "coord-1")
	spec := h.loadSpec(t)

	res, err := h.dispatcher.Run(
		context.Background(),
		"coord-1", "evt-dispatch-1",
		"domain", "schema_explorer",
		spec,
		"describe tf.incidents",
		"",
	)
	require.NoError(t, err)
	assert.Empty(t, res.Error, "happy path must not set Error")
	assert.False(t, res.Truncated, "200-rune cap is generous for a one-line summary")
	assert.NotEmpty(t, res.ChildSessionID)
	assert.Contains(t, res.Summary, "tf.incidents")
	assert.Contains(t, res.Summary, "severity")
	assert.Equal(t, 1, res.TurnsUsed)
}

// ----- T112: summary truncation -----

func TestDispatcher_Run_TruncatesSummary(t *testing.T) {
	// Specialist returns a 1KB blob; SummaryMaxTok = 200 from the
	// test skill must clip it and set Truncated.
	long := strings.Repeat("X", 2_000)
	h := newDispatchHarness(t, []models.ScriptedResponse{
		{Content: long},
	})
	h.makeCoordinator(t, "coord-truncate")
	spec := h.loadSpec(t)

	res, err := h.dispatcher.Run(
		context.Background(),
		"coord-truncate", "evt-1",
		"domain", "schema_explorer",
		spec,
		"long answer please",
		"",
	)
	require.NoError(t, err)
	assert.True(t, res.Truncated, "long summary must be flagged Truncated")
	assert.Equal(t, 200, len([]rune(res.Summary)),
		"Summary must be capped at SummaryMaxTok runes")
	assert.Empty(t, res.Error)
}

// ----- T103: SubAgentService tool declarations -----

func TestSubAgentService_ProviderShape(t *testing.T) {
	h := newDispatchHarness(t, nil)
	svc, err := NewSubAgentService(h.dispatcher, h.manager, h.skills)
	require.NoError(t, err)

	assert.Equal(t, SubAgentProviderName, svc.Name())
	toolsList := svc.Tools()
	require.Len(t, toolsList, 2)

	names := map[string]bool{}
	for _, tl := range toolsList {
		names[tl.Name()] = true
		assert.NotEmpty(t, tl.Description(),
			"tool %q missing description", tl.Name())
	}
	assert.True(t, names["subagent_list"], "subagent_list tool missing")
	assert.True(t, names["subagent_dispatch"], "subagent_dispatch tool missing")

	// subagent_dispatch declares params so the LLM knows the schema.
	// Concrete type assertion — tool.Tool interface alone doesn't
	// expose Declaration (it's on the internal runnableTool shape).
	var dispatch *subagentDispatchTool
	for _, tl := range toolsList {
		if d, ok := tl.(*subagentDispatchTool); ok {
			dispatch = d
		}
	}
	require.NotNil(t, dispatch)
	decl := dispatch.Declaration()
	require.NotNil(t, decl)
	assert.Equal(t, "subagent_dispatch", decl.Name)
	require.NotNil(t, decl.Parameters)
	assert.EqualValues(t, "OBJECT", decl.Parameters.Type)
	assert.ElementsMatch(t, []string{"skill", "role", "task"}, decl.Parameters.Required)
	assert.Contains(t, decl.Parameters.Properties, "skill")
	assert.Contains(t, decl.Parameters.Properties, "role")
	assert.Contains(t, decl.Parameters.Properties, "task")
	assert.Contains(t, decl.Parameters.Properties, "notes")

	// IsLongRunning: dispatch yes, list no.
	assert.True(t, dispatch.IsLongRunning())
	for _, tl := range toolsList {
		if tl.Name() == "subagent_list" {
			assert.False(t, tl.IsLongRunning())
		}
	}
}

// ----- T114: pre-flight refusal -----

func TestDispatcher_Run_PreflightRefusesOversizeTask(t *testing.T) {
	// task is so large it exceeds 50% of the cheap-model budget. The
	// dispatcher must refuse BEFORE creating a child session.
	h := newDispatchHarness(t, []models.ScriptedResponse{
		{Content: "should never run"},
	})
	h.makeCoordinator(t, "coord-preflight")
	spec := h.loadSpec(t)
	// Reduce the cheap-model budget so a moderate task trips the
	// 50% threshold without us writing a megabyte string.
	h.dispatcher.router.SetBudgets(map[string]int{
		"test-cheap-model": 200,
	}, 0)
	hugeTask := strings.Repeat("token ", 200) // ~300 estimated tokens

	res, err := h.dispatcher.Run(
		context.Background(),
		"coord-preflight", "evt-1",
		"domain", "schema_explorer",
		spec,
		hugeTask,
		"",
	)
	require.NoError(t, err)
	assert.Contains(t, res.Error, "exceed cheap-model budget",
		"pre-flight refusal must surface in Error")
	assert.Empty(t, res.Summary)
	assert.Empty(t, res.ChildSessionID,
		"refusal must NOT create a child session row")
	assert.Equal(t, 0, h.specialistLLM.Turns(),
		"specialist LLM must not be called on pre-flight refusal")
}
