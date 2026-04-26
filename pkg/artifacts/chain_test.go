//go:build duckdb_arrow

package artifacts_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hugr-lab/hugen/pkg/artifacts"
)

func TestManager_Chain_HappyPath(t *testing.T) {
	f := newVfixture(t)
	ctx := context.Background()

	// 3-link lineage: a (root) → b (derived from a) → c (from b).
	a, err := f.mgr.Publish(ctx, artifacts.PublishRequest{
		CallerSessionID: f.coordID,
		Source:          artifacts.PublishSource{InlineBytes: []byte("a")},
		Name:            "a", Type: "txt", Description: "root",
		Visibility: artifacts.VisibilityUser,
	})
	require.NoError(t, err)
	b, err := f.mgr.Publish(ctx, artifacts.PublishRequest{
		CallerSessionID: f.coordID,
		Source:          artifacts.PublishSource{InlineBytes: []byte("b")},
		Name:            "b", Type: "txt", Description: "derived",
		Visibility:  artifacts.VisibilityUser,
		DerivedFrom: a.ID,
	})
	require.NoError(t, err)
	c, err := f.mgr.Publish(ctx, artifacts.PublishRequest{
		CallerSessionID: f.coordID,
		Source:          artifacts.PublishSource{InlineBytes: []byte("c")},
		Name:            "c", Type: "txt", Description: "leaf",
		Visibility:  artifacts.VisibilityUser,
		DerivedFrom: b.ID,
	})
	require.NoError(t, err)

	chain, err := f.mgr.Chain(ctx, f.coordID, c.ID)
	require.NoError(t, err)
	require.Len(t, chain, 3)
	assert.Equal(t, a.ID, chain[0].ID, "oldest first")
	assert.Equal(t, b.ID, chain[1].ID)
	assert.Equal(t, c.ID, chain[2].ID, "leaf last")
}

func TestManager_Chain_NotCoordinator(t *testing.T) {
	f := newVfixture(t)
	ctx := context.Background()

	a, err := f.mgr.Publish(ctx, artifacts.PublishRequest{
		CallerSessionID: f.coordID,
		Source:          artifacts.PublishSource{InlineBytes: []byte("x")},
		Name:            "n", Type: "txt", Description: "d",
		Visibility: artifacts.VisibilityUser,
	})
	require.NoError(t, err)

	_, err = f.mgr.Chain(ctx, f.subAID, a.ID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, artifacts.ErrNotCoordinator), "got %v", err)
}

func TestManager_Chain_HiddenAncestor(t *testing.T) {
	f := newVfixture(t)
	ctx := context.Background()

	// Sub-A publishes self-scoped → coord can't see it. Then sub-A
	// publishes a parent-scoped child derived from the hidden one.
	hidden, err := f.mgr.Publish(ctx, artifacts.PublishRequest{
		CallerSessionID: f.subAID,
		Source:          artifacts.PublishSource{InlineBytes: []byte("secret")},
		Name:            "private", Type: "txt", Description: "self only",
	})
	require.NoError(t, err)
	visible, err := f.mgr.Publish(ctx, artifacts.PublishRequest{
		CallerSessionID: f.subAID,
		Source:          artifacts.PublishSource{InlineBytes: []byte("public")},
		Name:            "open", Type: "txt", Description: "parent-scope",
		Visibility:  artifacts.VisibilityParent,
		DerivedFrom: hidden.ID,
	})
	require.NoError(t, err)

	chain, err := f.mgr.Chain(ctx, f.coordID, visible.ID)
	require.NoError(t, err)
	require.Len(t, chain, 2, "depth preserved with placeholder")
	assert.Equal(t, hidden.ID, chain[0].ID, "ancestor id surfaced (it's a known string)")
	assert.Equal(t, "<hidden>", chain[0].Name, "name redacted")
	assert.Equal(t, visible.ID, chain[1].ID)
	assert.Equal(t, "open", chain[1].Name)
}

func TestManager_Chain_EntryInvisible(t *testing.T) {
	f := newVfixture(t)
	ctx := context.Background()

	// Sub-A publishes self-scoped → coord can't see → Chain on
	// that id returns ErrUnknownArtifact (collapse w/ existence
	// per FR-034).
	hidden, err := f.mgr.Publish(ctx, artifacts.PublishRequest{
		CallerSessionID: f.subAID,
		Source:          artifacts.PublishSource{InlineBytes: []byte("x")},
		Name:            "n", Type: "txt", Description: "self",
	})
	require.NoError(t, err)

	_, err = f.mgr.Chain(ctx, f.coordID, hidden.ID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, artifacts.ErrUnknownArtifact))
}
