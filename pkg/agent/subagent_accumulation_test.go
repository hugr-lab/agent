//go:build duckdb_arrow

package agent

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	adksession "google.golang.org/adk/session"

	"github.com/hugr-lab/hugen/internal/testenv"
	"github.com/hugr-lab/hugen/pkg/models"
	"github.com/hugr-lab/hugen/pkg/sessions"
	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
	"github.com/hugr-lab/hugen/pkg/skills"
	"github.com/hugr-lab/hugen/pkg/tools"
)

// hubBackedDispatchHarness mirrors dispatchHarness but wires a real
// hub via testenv so Dispatcher.Run persists child sessions with
// skill/role metadata, and session_notes_chain queries work
// end-to-end. Needed for the accumulation scenario: specialist A's
// note (authored under subagent session id) must render with a
// "[from <skill>/<role>]" prefix on specialist B's Snapshot.
type hubBackedDispatchHarness struct {
	manager       *sessions.Manager
	hub           *sessstore.Client
	skills        skills.Manager
	dispatcher    *Dispatcher
	specialistLLM *models.ScriptedLLM
}

func newHubBackedDispatchHarness(t *testing.T, script []models.ScriptedResponse) *hubBackedDispatchHarness {
	t.Helper()

	service, _ := testenv.Engine(t)
	logger := slog.New(slog.NewTextHandler(discardWriter{}, nil))

	testenv.RegisterAgent(t, service, "agt_ag01", "ag01", "accum-test-agent")

	skillsRoot := makeDispatchSkillsDir(t)
	skMgr, err := skills.NewFileManager(skillsRoot)
	require.NoError(t, err)

	tm := tools.New(nil)
	tm.AddProvider(tools.FakeProvider{N: "hugr-main", T: tools.FakeTools("schema_lookup")})

	hub, err := sessstore.New(service, sessstore.Options{
		AgentID: "agt_ag01", AgentShort: "ag01", Logger: logger,
	})
	require.NoError(t, err)

	sm, err := sessions.New(sessions.Config{
		Skills:       skMgr,
		Tools:        tm,
		Querier:      service,
		AgentID:      "agt_ag01",
		AgentShort:   "ag01",
		Constitution: "TEST",
		Logger:       logger,
	})
	require.NoError(t, err)

	specialist := models.NewScriptedLLM("test-cheap-model", script)
	router := models.NewRouterWithDefault(models.NewScriptedLLM("test-strong-model", nil))
	router.SetRoute(models.IntentToolCalling, specialist)
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

	return &hubBackedDispatchHarness{
		manager:       sm,
		hub:           hub,
		skills:        skMgr,
		dispatcher:    disp,
		specialistLLM: specialist,
	}
}

func (h *hubBackedDispatchHarness) makeCoordinator(t *testing.T, id string) {
	t.Helper()
	_, err := h.manager.Create(context.Background(), &adksession.CreateRequest{
		AppName:   "test-app",
		UserID:    "test-user",
		SessionID: id,
	})
	require.NoError(t, err)
}

func (h *hubBackedDispatchHarness) loadSpec(t *testing.T) skills.SubAgentSpec {
	t.Helper()
	sk, err := h.skills.Load(context.Background(), "domain")
	require.NoError(t, err)
	spec, ok := sk.SubAgents["schema_explorer"]
	require.True(t, ok)
	return spec
}

// TestDispatcher_AccumulationAcrossSpecialists covers spec 006 SC-003
// (T313): specialist A promotes a finding via memory_note(scope:
// "parent"); specialist B, dispatched later against the same
// coordinator, sees A's note via session_notes_chain with a
// "[from <skill>/<role>]" provenance prefix sourced from A's
// sessions.metadata.
//
// The test simulates the memory_note(scope: parent) write directly
// against the hub instead of driving the specialist through the
// _memory skill's tool — the critical plumbing (Dispatcher stamps
// __skill__/__role__ into child session metadata, chain view
// propagates the author id, renderer resolves the prefix) is exactly
// what we want to assert independently of the LLM loop.
func TestDispatcher_AccumulationAcrossSpecialists(t *testing.T) {
	h := newHubBackedDispatchHarness(t, []models.ScriptedResponse{
		{Content: "tf.incidents has 14 fields; severity is enum 1-3."},
		{Content: "query result for specialist B."},
	})
	h.makeCoordinator(t, "coord-accum")
	spec := h.loadSpec(t)

	// --- Dispatch specialist A ---
	resA, err := h.dispatcher.Run(
		context.Background(),
		"coord-accum", "evt-dispatch-A",
		"domain", "schema_explorer",
		spec,
		"describe tf.incidents",
		"",
	)
	require.NoError(t, err)
	require.Empty(t, resA.Error)
	require.NotEmpty(t, resA.ChildSessionID)
	specialistAID := resA.ChildSessionID

	// A promotes a finding up to the coordinator. Real memory_note
	// does exactly this write: session_id = parent (coord),
	// author_session_id = self (A).
	_, err = h.hub.AddNote(context.Background(), sessstore.Note{
		SessionID:       "coord-accum",
		AuthorSessionID: specialistAID,
		Content:         "tf.incidents.station_id is FK -> tf.stations",
	})
	require.NoError(t, err)

	// --- Dispatch specialist B (same coordinator) ---
	resB, err := h.dispatcher.Run(
		context.Background(),
		"coord-accum", "evt-dispatch-B",
		"domain", "schema_explorer",
		spec,
		"pick query for tf.incidents",
		"",
	)
	require.NoError(t, err)
	require.Empty(t, resB.Error)
	require.NotEmpty(t, resB.ChildSessionID)

	// --- Assert B's Snapshot picks up A's note with provenance ---
	specialistB, err := h.manager.Session(resB.ChildSessionID)
	require.NoError(t, err)
	// Clear any stale cache (B has just rendered its prompt during
	// the Run; force a re-read so subsequent hub writes would land).
	specialistB.InvalidateNotesCache()
	snap := specialistB.Snapshot()

	assert.Contains(t, snap.Prompt, "[from domain/schema_explorer]",
		"specialist B must see A's note with the author's skill/role prefix")
	assert.Contains(t, snap.Prompt, "tf.incidents.station_id is FK -> tf.stations",
		"specialist B must see A's promoted note body")

	// Sanity: A and B are distinct session ids and both point at the
	// same coordinator parent.
	_, err = h.manager.Session(specialistAID)
	require.NoError(t, err)
	assert.NotEqual(t, specialistAID, resB.ChildSessionID)
}
