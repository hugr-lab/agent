//go:build duckdb_arrow

package artifacts_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hugr-lab/hugen/internal/testenv"
	"github.com/hugr-lab/hugen/pkg/artifacts"
	artstore "github.com/hugr-lab/hugen/pkg/artifacts/store"
	artfs "github.com/hugr-lab/hugen/pkg/artifacts/storage/fs"
	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
)

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// fixture spins up an embedded engine + sessions store + active fs
// backend + the manager wired to all of them, and creates a single
// "root" coordinator session that tests publish into.
type fixture struct {
	t       *testing.T
	mgr     *artifacts.Manager
	sess    *sessstore.Client
	store   *artstore.Client
	dataDir string
	coordID string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(discardWriter{}, nil))
	service, _ := testenv.SharedEngine()
	require.NoError(t, artifacts.ResetSharedTables(t, context.Background(), service))

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

	mgr, err := artifacts.New(artifacts.Config{
		InlineBytesMax: 1 << 20,
	}, artifacts.Deps{
		Querier:       service,
		Storage:       be,
		SessionEvents: sess,
		Logger:        logger,
		AgentID:       "agt_ag01",
		AgentShort:    "ag01",
	})
	require.NoError(t, err)

	// Create a coordinator session — Publish writes the
	// artifact_published event onto it.
	coordID := "coord-art-1"
	_, err = sess.CreateSession(context.Background(), sessstore.Record{
		ID: coordID, AgentID: "agt_ag01", Status: "active",
		SessionType: sessstore.SessionTypeRoot,
	})
	require.NoError(t, err)

	return &fixture{
		t: t, mgr: mgr, sess: sess, store: store, dataDir: dataDir, coordID: coordID,
	}
}

func TestManager_Publish_InlineBytes_HappyPath(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	ref, err := f.mgr.Publish(ctx, artifacts.PublishRequest{
		CallerSessionID: f.coordID,
		Source:          artifacts.PublishSource{InlineBytes: []byte("hello world")},
		Name:            "greeting",
		Type:            "txt",
		Description:     "A simple greeting for tests",
		Tags:            []string{"smoke", "test"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, ref.ID)
	assert.Equal(t, "greeting", ref.Name)
	assert.Equal(t, "txt", ref.Type)
	assert.Equal(t, artifacts.VisibilitySelf, ref.Visibility) // default
	assert.Equal(t, int64(len("hello world")), ref.SizeBytes)
	assert.Equal(t, "fs", ref.StorageBackend)

	// 1 row in artifacts.
	rec, found, err := f.store.Get(ctx, ref.ID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "greeting", rec.Name)
	assert.Equal(t, "txt", rec.Type)
	assert.Equal(t, "self", rec.Visibility)
	assert.Equal(t, "session", rec.TTL)
	assert.Equal(t, "fs", rec.StorageBackend)
	assert.NotEmpty(t, rec.StorageKey)
	assert.Equal(t, []string{"smoke", "test"}, rec.Tags)
	assert.Equal(t, "A simple greeting for tests", rec.Description)
	assert.Equal(t, f.coordID, rec.SessionID)

	// 1 artifact_published event on the creator session.
	evs, err := f.sess.GetEvents(ctx, f.coordID)
	require.NoError(t, err)
	var seen bool
	for _, ev := range evs {
		if ev.EventType == sessstore.EventTypeArtifactPublished {
			seen = true
			assert.Equal(t, ref.ID, ev.Metadata["artifact_id"])
			assert.Equal(t, "greeting", ev.Metadata["name"])
			assert.Equal(t, "self", ev.Metadata["visibility"])
		}
	}
	assert.True(t, seen, "expected an artifact_published event on coord session")

	// Bytes physically present on disk under the configured dir.
	files, err := filepath.Glob(filepath.Join(f.dataDir, ref.ID+".*"))
	require.NoError(t, err)
	require.Len(t, files, 1)
}

func TestManager_Publish_ValidationFailures(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Empty description → ErrDescriptionRequired.
	_, err := f.mgr.Publish(ctx, artifacts.PublishRequest{
		CallerSessionID: f.coordID,
		Source:          artifacts.PublishSource{InlineBytes: []byte("x")},
		Name:            "n", Type: "txt", Description: "  ",
	})
	require.ErrorIs(t, err, artifacts.ErrDescriptionRequired)

	// Both sources set → ErrSourceAmbiguous.
	_, err = f.mgr.Publish(ctx, artifacts.PublishRequest{
		CallerSessionID: f.coordID,
		Source:          artifacts.PublishSource{Path: "/x", InlineBytes: []byte("y")},
		Name:            "n", Type: "txt", Description: "d",
	})
	require.ErrorIs(t, err, artifacts.ErrSourceAmbiguous)

	// Inline too large.
	_, err = f.mgr.Publish(ctx, artifacts.PublishRequest{
		CallerSessionID: f.coordID,
		Source:          artifacts.PublishSource{InlineBytes: make([]byte, 1<<21)}, // 2 MiB > 1 MiB cap
		Name:            "n", Type: "txt", Description: "d",
	})
	require.ErrorIs(t, err, artifacts.ErrInlineBytesTooLarge)

	// Invalid visibility.
	_, err = f.mgr.Publish(ctx, artifacts.PublishRequest{
		CallerSessionID: f.coordID,
		Source:          artifacts.PublishSource{InlineBytes: []byte("y")},
		Name:            "n", Type: "txt", Description: "d",
		Visibility: artifacts.Visibility("nope"),
	})
	require.ErrorIs(t, err, artifacts.ErrInvalidVisibility)

	// Invalid TTL.
	_, err = f.mgr.Publish(ctx, artifacts.PublishRequest{
		CallerSessionID: f.coordID,
		Source:          artifacts.PublishSource{InlineBytes: []byte("y")},
		Name:            "n", Type: "txt", Description: "d",
		TTL: artifacts.TTL("forever"),
	})
	require.ErrorIs(t, err, artifacts.ErrInvalidTTL)
}

func TestManager_Publish_PathSource(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Write source file to a temp dir.
	src := filepath.Join(t.TempDir(), "data.csv")
	require.NoError(t, writeFile(src, "id,value\n1,42\n2,99\n"))

	ref, err := f.mgr.Publish(ctx, artifacts.PublishRequest{
		CallerSessionID: f.coordID,
		Source:          artifacts.PublishSource{Path: src},
		Name:            "tiny dataset",
		Type:            "csv",
		Description:     "Two-row CSV for tests",
		Visibility:      artifacts.VisibilityParent,
		TTL:             artifacts.TTL7d,
	})
	require.NoError(t, err)
	assert.Equal(t, artifacts.VisibilityParent, ref.Visibility)

	// Stored row carries the original path for audit.
	rec, found, err := f.store.Get(ctx, ref.ID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, src, rec.OriginalPath)
	assert.Equal(t, "parent", rec.Visibility)
	assert.Equal(t, "7d", rec.TTL)
	assert.Equal(t, int64(len("id,value\n1,42\n2,99\n")), rec.SizeBytes)
}

func TestManager_AddGrant_UnknownArtifact(t *testing.T) {
	f := newFixture(t)
	err := f.mgr.AddGrant(context.Background(), "art_does_not_exist", "agt_ag01", "sess-x", f.coordID)
	require.ErrorIs(t, err, artifacts.ErrUnknownArtifact)
}

func TestManager_NameTools(t *testing.T) {
	f := newFixture(t)
	assert.Equal(t, "_artifacts", f.mgr.Name())
	tools := f.mgr.Tools()
	require.Len(t, tools, 6, "US1..US9 ship publish + info + list + visibility + remove + chain")
	names := make([]string, 0, len(tools))
	for _, tt := range tools {
		names = append(names, tt.Name())
	}
	assert.ElementsMatch(t, []string{
		"artifact_publish",
		"artifact_info",
		"artifact_list",
		"artifact_visibility",
		"artifact_remove",
		"artifact_chain",
	}, names)
}

// writeFile is a small helper around os.WriteFile with perms 0600.
func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}
