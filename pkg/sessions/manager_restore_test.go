//go:build duckdb_arrow

package sessions

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	adksession "google.golang.org/adk/session"

	agentstore "github.com/hugr-lab/hugen/pkg/agent/store"
	"github.com/hugr-lab/hugen/pkg/skills"
	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
	"github.com/hugr-lab/hugen/internal/testenv"
	"github.com/hugr-lab/hugen/pkg/tools"
)

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// newManagerWithHub wires a Manager against the embedded hugr engine
// and returns it along with the session-store client used to seed
// events directly. Skills manager has an autoload _sys skill for
// parity with production behaviour.
func newManagerWithHub(t *testing.T) (*Manager, *sessstore.Client) {
	t.Helper()
	service, _ := testenv.Engine(t)
	logger := slog.New(slog.NewTextHandler(discardWriter{}, nil))

	// Ensure the agent row exists; FKs on session_events point at it.
	reg, err := agentstore.New(service, agentstore.Options{
		AgentID: "agt_ag01", AgentShort: "ag01", Logger: logger,
	})
	require.NoError(t, err)
	require.NoError(t, reg.RegisterAgent(context.Background(), agentstore.Agent{
		ID: "agt_ag01", AgentTypeID: "hugr-data", ShortID: "ag01",
		Name: "restore-test-agent", Status: "active",
	}))

	root := makeSkillsDir(t)
	sk, err := skills.NewFileManager(root)
	require.NoError(t, err)

	tm := tools.New(nil)
	tm.AddProvider(tools.FakeProvider{N: "hugr-main", T: tools.FakeTools("demo_query")})
	tm.AddProvider(tools.FakeProvider{N: "_skills", T: tools.FakeTools("skill_list")})

	m, err := New(Config{
		Skills:       sk,
		Tools:        tm,
		Querier:      service,
		AgentID:      "agt_ag01",
		AgentShort:   "ag01",
		Constitution: "C",
		Logger:       logger,
	})
	require.NoError(t, err)

	hub, err := sessstore.New(service, sessstore.Options{
		AgentID: "agt_ag01", AgentShort: "ag01", Logger: logger,
	})
	require.NoError(t, err)
	return m, hub
}

// TestRestoreOpen_StubOnly verifies that a restart (simulated by a
// fresh Manager pointed at the same hub) creates session stubs
// without materialising events. The stub must carry the app_name
// written at Create time.
func TestRestoreOpen_StubOnly(t *testing.T) {
	m, hub := newManagerWithHub(t)
	ctx := context.Background()

	// Create a session in process #1.
	resp, err := m.Create(ctx, &adksession.CreateRequest{
		AppName: "hugr_agent", UserID: "u-1", SessionID: "sess-restore",
	})
	require.NoError(t, err)
	require.NotNil(t, resp.Session)

	// Seed two conversation events in hub.db — these represent what
	// the classifier would have flushed during a live session.
	_, err = hub.AppendEvent(ctx, sessstore.Event{
		SessionID: "sess-restore", EventType: sessstore.EventTypeUserMessage,
		Author: "u-1", Content: "привет",
	})
	require.NoError(t, err)
	_, err = hub.AppendEvent(ctx, sessstore.Event{
		SessionID: "sess-restore", EventType: sessstore.EventTypeLLMResponse,
		Author: "model", Content: "здравствуйте",
		Metadata: map[string]any{"model": "test", "prompt_tokens": 3, "completion_tokens": 5},
	})
	require.NoError(t, err)

	// Simulate a restart: wipe the in-memory sessions map and invoke
	// RestoreOpen against the same hub.db. Equivalent to what buildRuntime
	// does after a process restart.
	m.mu.Lock()
	m.sessions = map[string]*Session{}
	m.mu.Unlock()

	require.NoError(t, m.RestoreOpen(ctx))

	// Stub is there with right app_name; events not replayed yet.
	m.mu.RLock()
	sess, ok := m.sessions["sess-restore"]
	m.mu.RUnlock()
	require.True(t, ok)
	assert.Equal(t, "hugr_agent", sess.appName)
	assert.Equal(t, "u-1", sess.userID)
	assert.Equal(t, 0, sess.events.Len(), "events must not be replayed before Get")

	// Get triggers materialize — replays both events.
	got, err := m.Get(ctx, &adksession.GetRequest{
		AppName: "hugr_agent", UserID: "u-1", SessionID: "sess-restore",
	})
	require.NoError(t, err)
	require.NotNil(t, got.Session)

	// Conversation events materialised.
	assert.Equal(t, 2, sess.events.Len(), "user + llm events replayed")
	evs := sess.events.snapshot()
	require.Len(t, evs, 2)
	assert.Equal(t, "user", evs[0].Content.Role)
	assert.Equal(t, "привет", evs[0].Content.Parts[0].Text)
	assert.Equal(t, "model", evs[1].Content.Role)
	assert.Equal(t, "здравствуйте", evs[1].Content.Parts[0].Text)

	// Second Get is a no-op — events don't double-up.
	_, err = m.Get(ctx, &adksession.GetRequest{SessionID: "sess-restore"})
	require.NoError(t, err)
	assert.Equal(t, 2, sess.events.Len(), "materialize is idempotent")
}
