//go:build duckdb_arrow

package store_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hugr-lab/hugen/internal/testenv"
	artstore "github.com/hugr-lab/hugen/pkg/artifacts/store"
	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
)

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// fixture wires the artifacts store + sessions store + a tree of
// sessions (coord → sub-A, coord → sub-B) so visibility tests have
// something to traverse.
type fixture struct {
	t       *testing.T
	store   *artstore.Client
	sess    *sessstore.Client
	coordID string
	subAID  string
	subBID  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(discardWriter{}, nil))
	service, _ := testenv.Engine(t)

	store, err := artstore.New(service, artstore.Options{
		AgentID: "agt_ag01", AgentShort: "ag01", Logger: logger,
	})
	require.NoError(t, err)

	sess, err := sessstore.New(service, sessstore.Options{
		AgentID: "agt_ag01", AgentShort: "ag01", Logger: logger,
	})
	require.NoError(t, err)

	ctx := context.Background()
	_, err = sess.CreateSession(ctx, sessstore.Record{
		ID: "coord-view-1", AgentID: "agt_ag01", Status: "active",
		SessionType: sessstore.SessionTypeRoot,
	})
	require.NoError(t, err)
	_, err = sess.CreateSession(ctx, sessstore.Record{
		ID: "subA-view-1", AgentID: "agt_ag01", Status: "active",
		SessionType: sessstore.SessionTypeSubAgent, ParentSessionID: "coord-view-1",
	})
	require.NoError(t, err)
	_, err = sess.CreateSession(ctx, sessstore.Record{
		ID: "subB-view-1", AgentID: "agt_ag01", Status: "active",
		SessionType: sessstore.SessionTypeSubAgent, ParentSessionID: "coord-view-1",
	})
	require.NoError(t, err)

	return &fixture{
		t: t, store: store, sess: sess,
		coordID: "coord-view-1", subAID: "subA-view-1", subBID: "subB-view-1",
	}
}

func TestSessionArtifacts_VisibilityScopes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Coord publishes a self-scoped row.
	coordSelf := mustInsert(t, f, "art_coord_self", f.coordID, "self")
	// Sub-A publishes a parent-scoped row (visible to sub-A own + coord).
	subAParent := mustInsert(t, f, "art_subA_parent", f.subAID, "parent")
	// Sub-B publishes a graph-scoped row (visible to anyone in the graph).
	subBGraph := mustInsert(t, f, "art_subB_graph", f.subBID, "graph")
	// Coord publishes a user-scoped row.
	coordUser := mustInsert(t, f, "art_coord_user", f.coordID, "user")

	// Sub-A view: should see own (self), graph-scope from sub-B,
	// user-scope from coord. Should NOT see sub-A's siblings'
	// parent-scoped rows (sub-B doesn't have a parent scoped one).
	rows, err := f.store.SessionArtifacts(ctx, f.subAID, artstore.SessionArtifactsFilter{Limit: 50})
	require.NoError(t, err)
	ids := idsOf(rows)
	assert.NotContains(t, ids, coordSelf, "sub-A must NOT see coord's self-scope row")
	assert.Contains(t, ids, subBGraph, "sub-A must see sub-B's graph-scope row")
	assert.Contains(t, ids, coordUser, "sub-A must see coord's user-scope row")
	// sub-A own parent-scoped row is visible via 'self'.
	assert.Contains(t, ids, subAParent, "sub-A must see own parent-scoped row")

	// Coord view: should see own (self+user), parent-scope from
	// sub-A (sub-A is depth 1), graph-scope from sub-B.
	rows, err = f.store.SessionArtifacts(ctx, f.coordID, artstore.SessionArtifactsFilter{Limit: 50})
	require.NoError(t, err)
	ids = idsOf(rows)
	assert.Contains(t, ids, coordSelf)
	assert.Contains(t, ids, coordUser)
	assert.Contains(t, ids, subAParent, "coord must see sub-A's parent-scope row")
	assert.Contains(t, ids, subBGraph, "coord must see sub-B's graph-scope row")
}

func TestSessionArtifactByID_AdminUserOnly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Self-scope row → admin must NOT see it.
	selfID := mustInsert(t, f, "art_admin_self", f.coordID, "self")
	rec, ok, err := f.store.SessionArtifactByID(ctx, "", selfID)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, "", rec.ID)

	// User-scope row → admin sees it, visible_via = user.
	userID := mustInsert(t, f, "art_admin_user", f.coordID, "user")
	rec, ok, err = f.store.SessionArtifactByID(ctx, "", userID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, userID, rec.ID)
	assert.Equal(t, "user", rec.VisibleVia)
}

func TestSessionArtifactByID_GrantOverlay(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Coord publishes a self-scoped row → invisible to sub-B.
	id := mustInsert(t, f, "art_grant_target", f.coordID, "self")
	rec, ok, err := f.store.SessionArtifactByID(ctx, f.subBID, id)
	require.NoError(t, err)
	assert.False(t, ok, "sub-B must not see coord's self-scope row")
	assert.Equal(t, "", rec.ID)

	// Add a grant for sub-B → now visible.
	require.NoError(t, f.store.AddGrant(ctx, artstore.GrantRecord{
		ArtifactID: id, AgentID: "agt_ag01", SessionID: f.subBID, GrantedBy: f.coordID,
	}))
	rec, ok, err = f.store.SessionArtifactByID(ctx, f.subBID, id)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "grant", rec.VisibleVia)
}

func mustInsert(t *testing.T, f *fixture, id, sessID, vis string) string {
	t.Helper()
	_, err := f.store.Insert(context.Background(), artstore.Record{
		ID:             id,
		AgentID:        "agt_ag01",
		Name:           id,
		Type:           "csv",
		StorageKey:     id + ".csv",
		StorageBackend: "fs",
		Description:    "row " + id,
		SessionID:      sessID,
		Visibility:     vis,
		TTL:            "session",
	})
	require.NoError(t, err)
	return id
}

func idsOf(rows []artstore.Record) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}
