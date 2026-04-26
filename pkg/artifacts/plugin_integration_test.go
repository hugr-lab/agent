//go:build duckdb_arrow

package artifacts

import (
	"context"
	"iter"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/adk/agent"
	adksession "google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/hugr-lab/hugen/internal/testenv"
	artfs "github.com/hugr-lab/hugen/pkg/artifacts/storage/fs"
	artstore "github.com/hugr-lab/hugen/pkg/artifacts/store"
	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
)

// ─────────────────────────────────────────────────────────────────
// Minimal fake InvocationContext for the user-upload plugin.
//
// We only exercise Session() and the embedded context.Context;
// every other method on agent.InvocationContext panics so this
// stays lock-step with the contract — if ADK starts depending on
// some other accessor here, the test fails loudly instead of
// silently returning zeroes.
// ─────────────────────────────────────────────────────────────────

type fakeSession struct {
	id     string
	user   string
	app    string
}

func (s *fakeSession) ID() string                       { return s.id }
func (s *fakeSession) AppName() string                  { return s.app }
func (s *fakeSession) UserID() string                   { return s.user }
func (s *fakeSession) State() adksession.State          { return nil }
func (s *fakeSession) Events() adksession.Events        { return nil }
func (s *fakeSession) LastUpdateTime() time.Time        { return time.Time{} }

type fakeInvCtx struct {
	context.Context
	sess adksession.Session
}

func (f *fakeInvCtx) Agent() agent.Agent                                { panic("not used") }
func (f *fakeInvCtx) Artifacts() agent.Artifacts                        { panic("not used") }
func (f *fakeInvCtx) Memory() agent.Memory                              { panic("not used") }
func (f *fakeInvCtx) Session() adksession.Session                       { return f.sess }
func (f *fakeInvCtx) InvocationID() string                              { return "test-inv" }
func (f *fakeInvCtx) Branch() string                                    { return "" }
func (f *fakeInvCtx) UserContent() *genai.Content                       { return nil }
func (f *fakeInvCtx) RunConfig() *agent.RunConfig                       { return nil }
func (f *fakeInvCtx) EndInvocation()                                    {}
func (f *fakeInvCtx) Ended() bool                                       { return false }
func (f *fakeInvCtx) WithContext(ctx context.Context) agent.InvocationContext {
	cp := *f
	cp.Context = ctx
	return &cp
}

// silence unused-import nags for iter when adksession.Events drops
var _ = func() iter.Seq[any] { return nil }

// ─────────────────────────────────────────────────────────────────
// Test fixture (mirrors publish_test.go::newFixture but with the
// user-upload plugin's call site exercised).
// ─────────────────────────────────────────────────────────────────

type pluginFixture struct {
	mgr     *Manager
	sess    *sessstore.Client
	store   *artstore.Client
	ctx     context.Context
	rootID  string
}

func newPluginFixture(t *testing.T) *pluginFixture {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(discardW{}, nil))
	service, _ := testenv.SharedEngine()
	require.NoError(t, ResetSharedTables(context.Background(), service))

	sess, err := sessstore.New(service, sessstore.Options{
		AgentID: "agt_ag01", AgentShort: "ag01", Logger: logger,
	})
	require.NoError(t, err)

	store, err := artstore.New(service, artstore.Options{
		AgentID: "agt_ag01", AgentShort: "ag01", Logger: logger,
	})
	require.NoError(t, err)

	dataDir := filepath.Join(t.TempDir(), "artifacts")
	be, err := artfs.New(artfs.Config{Dir: dataDir})
	require.NoError(t, err)

	mgr, err := New(Config{
		InlineBytesMax:          1 << 20,
		UploadDefaultVisibility: VisibilityUser,
		UploadDefaultTTL:        TTL7d,
	}, Deps{
		Querier:       service,
		Storage:       be,
		SessionEvents: sess,
		Logger:        logger,
		AgentID:       "agt_ag01",
		AgentShort:    "ag01",
	})
	require.NoError(t, err)

	rootID := "root-upload-1"
	_, err = sess.CreateSession(context.Background(), sessstore.Record{
		ID: rootID, AgentID: "agt_ag01", Status: "active",
		SessionType: sessstore.SessionTypeRoot,
	})
	require.NoError(t, err)

	return &pluginFixture{mgr: mgr, sess: sess, store: store, ctx: context.Background(), rootID: rootID}
}

type discardW struct{}

func (discardW) Write(p []byte) (int, error) { return len(p), nil }

// ─────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────

func TestUserUploadPlugin_PublishesAndRewrites(t *testing.T) {
	f := newPluginFixture(t)

	msg := &genai.Content{
		Role: "user",
		Parts: []*genai.Part{
			{Text: "Please summarise this report:"},
			{InlineData: &genai.Blob{
				DisplayName: "q1-incidents.csv",
				MIMEType:    "text/csv",
				Data:        []byte("station,severity\n1,high\n2,low\n"),
			}},
		},
	}
	invCtx := &fakeInvCtx{
		Context: f.ctx,
		sess:    &fakeSession{id: f.rootID, user: "user-1", app: "agt_ag01"},
	}

	mutated, err := f.mgr.onUserMessage(invCtx, msg)
	require.NoError(t, err)
	require.NotNil(t, mutated, "callback must return modified msg when uploads were processed")

	// Part 0 (text) untouched.
	assert.Equal(t, "Please summarise this report:", mutated.Parts[0].Text)
	// Part 1 (was InlineData) replaced with rich placeholder.
	require.Nil(t, mutated.Parts[1].InlineData, "blob should be removed")
	placeholder := mutated.Parts[1].Text
	require.Contains(t, placeholder, "[user-upload]")
	require.Contains(t, placeholder, "name: q1-incidents.csv")
	require.Contains(t, placeholder, "type: csv")
	require.Contains(t, placeholder, "mime: text/csv")
	// Local path is set by fs backend — must be present and absolute-ish.
	require.Contains(t, placeholder, "local_path: ")
	for _, line := range strings.Split(placeholder, "\n") {
		if strings.HasPrefix(line, "local_path: ") {
			path := strings.TrimPrefix(line, "local_path: ")
			assert.True(t, filepath.IsAbs(path), "local_path should be absolute, got %q", path)
		}
	}

	// Artifact row landed with operator-configured visibility.
	rows, err := f.store.ListByAgent(f.ctx, artstore.ListFilter{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "q1-incidents.csv", rows[0].Name)
	assert.Equal(t, "csv", rows[0].Type)
	assert.Equal(t, string(VisibilityUser), rows[0].Visibility)
	assert.Equal(t, string(TTL7d), rows[0].TTL)
}

func TestUserUploadPlugin_NoInlineData_NoOp(t *testing.T) {
	f := newPluginFixture(t)

	msg := &genai.Content{
		Role: "user",
		Parts: []*genai.Part{
			{Text: "just text"},
			{FileData: &genai.FileData{
				FileURI: "https://example.com/x.csv", MIMEType: "text/csv",
			}},
		},
	}
	invCtx := &fakeInvCtx{
		Context: f.ctx,
		sess:    &fakeSession{id: f.rootID, user: "user-1", app: "agt_ag01"},
	}

	out, err := f.mgr.onUserMessage(invCtx, msg)
	require.NoError(t, err)
	assert.Nil(t, out, "no InlineData → no mutation → callback returns nil")
	assert.Equal(t, "just text", msg.Parts[0].Text)
	require.NotNil(t, msg.Parts[1].FileData, "FileURI is passed through, not auto-published")

	rows, err := f.store.ListByAgent(f.ctx, artstore.ListFilter{})
	require.NoError(t, err)
	assert.Len(t, rows, 0, "no artifact should be created from FileData")
}

func TestUserUploadPlugin_EmitsUserUploadSourceMeta(t *testing.T) {
	f := newPluginFixture(t)

	msg := &genai.Content{Parts: []*genai.Part{
		{InlineData: &genai.Blob{
			DisplayName: "notes.txt", MIMEType: "text/plain", Data: []byte("hello"),
		}},
	}}
	invCtx := &fakeInvCtx{
		Context: f.ctx,
		sess:    &fakeSession{id: f.rootID, user: "user-1", app: "agt_ag01"},
	}
	_, err := f.mgr.onUserMessage(invCtx, msg)
	require.NoError(t, err)

	// Pull artifact_published events on the root session and assert
	// metadata.source == "user_upload".
	events, err := f.sess.GetEvents(f.ctx, f.rootID)
	require.NoError(t, err)
	found := false
	for _, ev := range events {
		if ev.EventType != sessstore.EventTypeArtifactPublished {
			continue
		}
		found = true
		require.NotNil(t, ev.Metadata, "artifact_published must carry metadata")
		src, _ := ev.Metadata["source"].(string)
		assert.Equal(t, "user_upload", src, "metadata.source must be tagged user_upload")
	}
	assert.True(t, found, "expected at least one artifact_published event")
}

// TestUserUploadPlugin_PublishFailure_SwapsErrorPlaceholder is the
// regression test for the BLOCKER review item: when Publish fails
// (here: blob exceeds InlineBytesMax), the plugin MUST replace the
// part with an error placeholder rather than leaving the raw blob
// for the LLM to consume. Otherwise an over-cap upload would defeat
// the whole point of the registry.
func TestUserUploadPlugin_PublishFailure_SwapsErrorPlaceholder(t *testing.T) {
	f := newPluginFixture(t)

	// Tighten the cap so a small blob trips it deterministically.
	f.mgr.cfg.InlineBytesMax = 4

	msg := &genai.Content{Parts: []*genai.Part{
		{InlineData: &genai.Blob{
			DisplayName: "too-big.bin",
			MIMEType:    "application/octet-stream",
			Data:        []byte("longer than 4 bytes — should be rejected"),
		}},
	}}
	invCtx := &fakeInvCtx{
		Context: f.ctx,
		sess:    &fakeSession{id: f.rootID, user: "user-1", app: "agt_ag01"},
	}

	mutated, err := f.mgr.onUserMessage(invCtx, msg)
	require.NoError(t, err)
	require.NotNil(t, mutated, "callback must mutate even on failure (replace part with error placeholder)")

	require.Nil(t, mutated.Parts[0].InlineData, "raw blob must NOT survive — bytes can't reach the LLM")
	placeholder := mutated.Parts[0].Text
	require.Contains(t, placeholder, "[user-upload-failed]")
	require.Contains(t, placeholder, "name: too-big.bin")
	require.Contains(t, placeholder, "reason:")

	// Sanity: nothing landed in the registry.
	rows, err := f.store.ListByAgent(f.ctx, artstore.ListFilter{})
	require.NoError(t, err)
	assert.Len(t, rows, 0, "failed upload must NOT create an artifact row")
}
