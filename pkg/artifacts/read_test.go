//go:build duckdb_arrow

package artifacts_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hugr-lab/hugen/pkg/artifacts"
)

func TestManager_OpenReader_HappyPath(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	ref, err := f.mgr.Publish(ctx, artifacts.PublishRequest{
		CallerSessionID: f.coordID,
		Source:          artifacts.PublishSource{InlineBytes: []byte("hello world")},
		Name:            "greeting",
		Type:            "txt",
		Description:     "tiny test artifact",
		Visibility:      artifacts.VisibilityUser,
	})
	require.NoError(t, err)

	rc, stat, err := f.mgr.OpenReader(ctx, "", ref.ID)
	require.NoError(t, err)
	defer rc.Close()
	body, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, []byte("hello world"), body)
	assert.Equal(t, int64(len("hello world")), stat.Size)
}

func TestManager_OpenReader_AdminUserOnly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// self-visibility artifact: NOT user-scoped → admin endpoint
	// (empty session) must not see it.
	ref, err := f.mgr.Publish(ctx, artifacts.PublishRequest{
		CallerSessionID: f.coordID,
		Source:          artifacts.PublishSource{InlineBytes: []byte("private")},
		Name:            "private-note",
		Type:            "txt",
		Description:     "self only",
		Visibility:      artifacts.VisibilitySelf,
	})
	require.NoError(t, err)

	_, _, err = f.mgr.OpenReader(ctx, "", ref.ID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, artifacts.ErrUnknownArtifact),
		"admin endpoint must collapse non-user-scope into ErrUnknownArtifact, got %v", err)

	// Same artifact opened as the creator session → visible.
	rc, _, err := f.mgr.OpenReader(ctx, f.coordID, ref.ID)
	require.NoError(t, err)
	rc.Close()
}

func TestManager_OpenReader_UnknownID(t *testing.T) {
	f := newFixture(t)
	_, _, err := f.mgr.OpenReader(context.Background(), "", "art_does_not_exist")
	require.Error(t, err)
	assert.True(t, errors.Is(err, artifacts.ErrUnknownArtifact))
}

func TestManager_Info_HappyPath(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	ref, err := f.mgr.Publish(ctx, artifacts.PublishRequest{
		CallerSessionID: f.coordID,
		Source:          artifacts.PublishSource{InlineBytes: []byte("data,1\n")},
		Name:            "row",
		Type:            "csv",
		Description:     "single row csv",
		Visibility:      artifacts.VisibilityUser,
		Tags:            []string{"smoke"},
	})
	require.NoError(t, err)

	d, err := f.mgr.Info(ctx, "", ref.ID)
	require.NoError(t, err)
	assert.Equal(t, ref.ID, d.ID)
	assert.Equal(t, "row", d.Name)
	assert.Equal(t, "csv", d.Type)
	assert.Equal(t, artifacts.VisibilityUser, d.Visibility)
	assert.Equal(t, "single row csv", d.Description)
	assert.Equal(t, []string{"smoke"}, d.Tags)
	assert.Equal(t, "fs", d.StorageBackend)
	assert.Equal(t, artifacts.TTLSession, d.TTL)
	assert.Equal(t, f.coordID, d.SessionID)
}

func TestManager_Info_VisibilityMissed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	ref, err := f.mgr.Publish(ctx, artifacts.PublishRequest{
		CallerSessionID: f.coordID,
		Source:          artifacts.PublishSource{InlineBytes: []byte("x")},
		Name:            "n",
		Type:            "txt",
		Description:     "self",
		Visibility:      artifacts.VisibilitySelf,
	})
	require.NoError(t, err)

	_, err = f.mgr.Info(ctx, "", ref.ID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, artifacts.ErrUnknownArtifact))
}
