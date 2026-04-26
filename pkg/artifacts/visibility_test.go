//go:build duckdb_arrow

package artifacts_test

import (
	"context"
	"errors"
	"log/slog"
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

// vfixture wires the registry against a 3-level session tree so
// visibility tests can traverse parent / graph scopes.
type vfixture struct {
	t       *testing.T
	mgr     *artifacts.Manager
	store   *artstore.Client
	sess    *sessstore.Client
	coordID string
	subAID  string
	subBID  string
}

func newVfixture(t *testing.T) *vfixture {
	return newVfixtureCfg(t, artifacts.Config{InlineBytesMax: 1 << 20})
}

// newVfixtureCfg lets a test override the manager Config (used by
// the cleanup tests to flip TTLSessionGrace).
func newVfixtureCfg(t *testing.T, cfg artifacts.Config) *vfixture {
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

	be, err := artfs.New(artfs.Config{Dir: filepath.Join(t.TempDir(), "art")})
	require.NoError(t, err)

	if cfg.InlineBytesMax == 0 {
		cfg.InlineBytesMax = 1 << 20
	}
	mgr, err := artifacts.New(cfg, artifacts.Deps{
		Querier: service, Storage: be, SessionEvents: sess,
		Logger: logger, AgentID: "agt_ag01", AgentShort: "ag01",
	})
	require.NoError(t, err)

	ctx := context.Background()
	mustSession(t, sess, "coord-vis", "", sessstore.SessionTypeRoot)
	mustSession(t, sess, "subA-vis", "coord-vis", sessstore.SessionTypeSubAgent)
	mustSession(t, sess, "subB-vis", "coord-vis", sessstore.SessionTypeSubAgent)
	_ = ctx
	return &vfixture{
		t: t, mgr: mgr, store: store, sess: sess,
		coordID: "coord-vis", subAID: "subA-vis", subBID: "subB-vis",
	}
}

func mustSession(t *testing.T, c *sessstore.Client, id, parent, kind string) {
	t.Helper()
	_, err := c.CreateSession(context.Background(), sessstore.Record{
		ID: id, AgentID: "agt_ag01", Status: "active",
		SessionType: kind, ParentSessionID: parent,
	})
	require.NoError(t, err)
}

func TestManager_ListVisible_ParentScope(t *testing.T) {
	f := newVfixture(t)
	ctx := context.Background()

	// Sub-A publishes parent-scope. Coord must see it via parent_rows.
	ref, err := f.mgr.Publish(ctx, artifacts.PublishRequest{
		CallerSessionID: f.subAID,
		Source:          artifacts.PublishSource{InlineBytes: []byte("ay")},
		Name:            "subA-out", Type: "txt",
		Description: "from sub-A", Visibility: artifacts.VisibilityParent,
	})
	require.NoError(t, err)

	refs, err := f.mgr.ListVisible(ctx, f.coordID, artifacts.ListFilter{})
	require.NoError(t, err)
	ids := refIDs(refs)
	assert.Contains(t, ids, ref.ID, "coord must see sub-A's parent-scope artifact")

	// Sibling sub-B must NOT see it.
	refs, err = f.mgr.ListVisible(ctx, f.subBID, artifacts.ListFilter{})
	require.NoError(t, err)
	assert.NotContains(t, refIDs(refs), ref.ID, "sibling must not see sub-A's parent-scope artifact")
}

func TestManager_WidenVisibility_HappyPath(t *testing.T) {
	f := newVfixture(t)
	ctx := context.Background()

	// Sub-A publishes parent-scope so coord sees it through the
	// view's parent_rows branch. (visibility=self by design hides
	// the artifact from everyone except sub_A — coord should NOT
	// be able to widen what sub_A explicitly marked private.)
	ref, err := f.mgr.Publish(ctx, artifacts.PublishRequest{
		CallerSessionID: f.subAID,
		Source:          artifacts.PublishSource{InlineBytes: []byte("for parent")},
		Name:            "draft", Type: "txt", Description: "draft for coord review",
		Visibility: artifacts.VisibilityParent,
	})
	require.NoError(t, err)

	require.NoError(t, f.mgr.WidenVisibility(ctx, f.coordID, ref.ID, artifacts.VisibilityUser, nil))

	// Now admin endpoint (empty session) sees it.
	d, err := f.mgr.Info(ctx, "", ref.ID)
	require.NoError(t, err)
	assert.Equal(t, artifacts.VisibilityUser, d.Visibility)
}

func TestManager_WidenVisibility_NotCoordinator(t *testing.T) {
	f := newVfixture(t)
	ctx := context.Background()

	ref, err := f.mgr.Publish(ctx, artifacts.PublishRequest{
		CallerSessionID: f.coordID,
		Source:          artifacts.PublishSource{InlineBytes: []byte("x")},
		Name:            "n", Type: "txt", Description: "owned by coord",
	})
	require.NoError(t, err)

	// Sub-A tries to widen → ErrNotCoordinator. Coord-gate fires
	// before the visibility resolution, so the response is the
	// same regardless of whether the artifact would be visible
	// to sub-A through the view.
	err = f.mgr.WidenVisibility(ctx, f.subAID, ref.ID, artifacts.VisibilityUser, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, artifacts.ErrNotCoordinator), "got %v", err)
}

func TestManager_WidenVisibility_Narrowing(t *testing.T) {
	f := newVfixture(t)
	ctx := context.Background()

	ref, err := f.mgr.Publish(ctx, artifacts.PublishRequest{
		CallerSessionID: f.coordID,
		Source:          artifacts.PublishSource{InlineBytes: []byte("x")},
		Name:            "n", Type: "txt", Description: "user-scope start",
		Visibility: artifacts.VisibilityUser,
	})
	require.NoError(t, err)

	// Trying to narrow user → parent must fail.
	err = f.mgr.WidenVisibility(ctx, f.coordID, ref.ID, artifacts.VisibilityParent, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, artifacts.ErrVisibilityNarrowing), "got %v", err)
}

func TestManager_WidenVisibility_Grant(t *testing.T) {
	f := newVfixture(t)
	ctx := context.Background()

	// Coord publishes self-scoped → grant access to sub-B.
	ref, err := f.mgr.Publish(ctx, artifacts.PublishRequest{
		CallerSessionID: f.coordID,
		Source:          artifacts.PublishSource{InlineBytes: []byte("x")},
		Name:            "n", Type: "txt", Description: "for sub-B",
	})
	require.NoError(t, err)

	require.NoError(t, f.mgr.WidenVisibility(ctx, f.coordID, ref.ID, "", &artifacts.GrantTarget{
		SessionID: f.subBID,
	}))

	// Sub-B now sees the artifact via grant overlay.
	refs, err := f.mgr.ListVisible(ctx, f.subBID, artifacts.ListFilter{})
	require.NoError(t, err)
	assert.Contains(t, refIDs(refs), ref.ID)
}

func TestManager_Remove_OwnArtifact(t *testing.T) {
	f := newVfixture(t)
	ctx := context.Background()

	ref, err := f.mgr.Publish(ctx, artifacts.PublishRequest{
		CallerSessionID: f.subAID,
		Source:          artifacts.PublishSource{InlineBytes: []byte("dispose me")},
		Name:            "tmp", Type: "txt", Description: "tmp",
	})
	require.NoError(t, err)

	require.NoError(t, f.mgr.Remove(ctx, f.subAID, ref.ID))

	_, err = f.mgr.Info(ctx, f.subAID, ref.ID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, artifacts.ErrUnknownArtifact))
}

func TestManager_Remove_NotAuthorised(t *testing.T) {
	f := newVfixture(t)
	ctx := context.Background()

	// Sub-A publishes parent-scope → coord can SEE it but cannot
	// remove (not user-visibility, not the creator).
	ref, err := f.mgr.Publish(ctx, artifacts.PublishRequest{
		CallerSessionID: f.subAID,
		Source:          artifacts.PublishSource{InlineBytes: []byte("x")},
		Name:            "n", Type: "txt", Description: "parent-only",
		Visibility: artifacts.VisibilityParent,
	})
	require.NoError(t, err)

	err = f.mgr.Remove(ctx, f.coordID, ref.ID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, artifacts.ErrNotAuthorisedToRemove), "got %v", err)
}

func TestManager_Remove_CoordOnUser(t *testing.T) {
	f := newVfixture(t)
	ctx := context.Background()

	// Sub-A publishes a user-scope artifact; coord may remove it.
	ref, err := f.mgr.Publish(ctx, artifacts.PublishRequest{
		CallerSessionID: f.subAID,
		Source:          artifacts.PublishSource{InlineBytes: []byte("public")},
		Name:            "open", Type: "txt", Description: "user scope",
		Visibility: artifacts.VisibilityUser,
	})
	require.NoError(t, err)

	require.NoError(t, f.mgr.Remove(ctx, f.coordID, ref.ID))
}

func refIDs(refs []artifacts.ArtifactRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.ID)
	}
	return out
}
