//go:build duckdb_arrow

package sessions

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/adk/model"
	adksession "google.golang.org/adk/session"
	"google.golang.org/genai"

	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
)

// TestSeqRace_ClassifierVsLoadSkill replicates the live-agent
// failure mode that swallowed the `tool_call subagent_dispatch` row
// during US1 manual smoke testing: the classifier (async drain) and
// Session.LoadSkill (sync writer for skill_loaded events) both
// claimed `seq = max(seq) + 1` concurrently, so one INSERT silently
// overwrote the other's slot in the transcript ordering.
//
// This test stands up the full classifier → Manager → Session write
// pipeline against a real hub, fires a burst of FunctionCall envelopes
// (mimicking what ADK emits when the LLM calls a long-running tool
// like subagent_dispatch) interleaved with concurrent skill_load
// invocations, then asserts every row landed with a unique seq + the
// expected counts (no events lost).
func TestSeqRace_ClassifierVsLoadSkill(t *testing.T) {
	m, hub := newManagerWithHub(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Wire the classifier the same way cmd/agent/runtime.go does:
	// Manager → AttachManager so writes funnel through the per-session
	// lock. Without this the test would reliably produce seq
	// collisions (the bug).
	cls := NewClassifierWithHub(hub, m.logger, DefaultClassifierBuffer)
	cls.AttachManager(m)
	go cls.Run(ctx)

	_, err := m.Create(ctx, &adksession.CreateRequest{
		AppName: "hugr_agent", UserID: "u-1", SessionID: "sess-cls-race",
	})
	require.NoError(t, err)
	sess, err := m.Session("sess-cls-race")
	require.NoError(t, err)

	const (
		toolCallEnvelopes = 15 // each emits 1 tool_call row via Classify
		loadSkillCalls    = 15 // each emits 1 skill_loaded row sync
	)

	// Push tool_call envelopes through the classifier in one
	// goroutine; in parallel pound LoadSkill in another. The Manager
	// already pre-loads `_sys` on Create — repeat loads are no-ops on
	// the LoadSkill side (RemoveSkill returns false the second time),
	// so use a small set of distinct skill names to actually generate
	// skill_loaded events.
	skillNames := make([]string, loadSkillCalls)
	for i := 0; i < loadSkillCalls; i++ {
		skillNames[i] = "skill-" + itoa(i)
	}
	// Have the manager's skill catalog return at least these — the
	// test fixture's makeSkillsDir only ships `_sys`, but LoadSkill
	// goes through the file manager which returns ErrNotFound for
	// unknown names. We work around this by writing the skill_loaded
	// rows directly via Session.AppendEvent (same code path the sync
	// LoadSkill uses internally; we just bypass the catalog lookup).

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < toolCallEnvelopes; i++ {
			env := Envelope{
				SessionID: "sess-cls-race",
				Event: &adksession.Event{
					Author: "agent",
					LLMResponse: model.LLMResponse{
						Content: &genai.Content{
							Role: "model",
							Parts: []*genai.Part{{
								FunctionCall: &genai.FunctionCall{
									Name: "subagent_dispatch",
									ID:   "call-" + itoa(i),
									Args: map[string]any{"skill": "hugr-data", "role": "schema_explorer", "task": "ping"},
								},
							}},
						},
					},
					LongRunningToolIDs: []string{"call-" + itoa(i)},
				},
			}
			require.True(t, cls.Publish(env), "channel should accept envelope %d", i)
		}
	}()

	go func() {
		defer wg.Done()
		for i, name := range skillNames {
			_, err := sess.AppendEvent(ctx, sessstore.Event{
				EventType: sessstore.EventTypeSkillLoaded,
				Author:    sess.id,
				Content:   name,
				Metadata:  map[string]any{"skill": name},
			}, "")
			require.NoError(t, err, "skill_load %d", i)
		}
	}()

	wg.Wait()

	// Drain classifier so every tool_call envelope is persisted.
	require.NoError(t, cls.Drain(ctx, 10*time.Second))

	rows, err := hub.GetEvents(ctx, "sess-cls-race")
	require.NoError(t, err)

	// Counts: every classifier envelope produced exactly one
	// tool_call row, every LoadSkill produced exactly one
	// skill_loaded row. Without the per-session writeMu the live
	// agent regularly lost one or more rows to seq collisions.
	// Note: newManagerWithHub autoloads one `_sys` skill on Create,
	// so we filter skill_loaded rows by the "skill-" prefix the test
	// itself wrote.
	var nToolCalls, nTestSkillLoads int
	seen := map[int]bool{}
	for _, ev := range rows {
		assert.False(t, seen[ev.Seq], "seq %d collided — race is back", ev.Seq)
		seen[ev.Seq] = true
		switch ev.EventType {
		case sessstore.EventTypeToolCall:
			nToolCalls++
		case sessstore.EventTypeSkillLoaded:
			if strings.HasPrefix(ev.Content, "skill-") {
				nTestSkillLoads++
			}
		}
	}
	assert.Equal(t, toolCallEnvelopes, nToolCalls,
		"all %d classifier tool_call rows must persist", toolCallEnvelopes)
	assert.Equal(t, loadSkillCalls, nTestSkillLoads,
		"all %d sync skill_loaded rows must persist", loadSkillCalls)
	assert.Equal(t, len(rows), len(seen),
		"every transcript seq must be distinct")
}

// itoa is a tiny stand-in for strconv.Itoa to keep the imports lean
// in this small test file.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := []byte{}
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	if neg {
		digits = append([]byte{'-'}, digits...)
	}
	return string(digits)
}
