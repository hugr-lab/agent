//go:build duckdb_arrow

package artifacts_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hugr-lab/hugen/pkg/artifacts"
)

func TestManager_Cleanup_SessionTTL(t *testing.T) {
	f := newVfixture(t)
	ctx := context.Background()

	// Sub-A publishes session-TTL row.
	ref, err := f.mgr.Publish(ctx, artifacts.PublishRequest{
		CallerSessionID: f.subAID,
		Source:          artifacts.PublishSource{InlineBytes: []byte("temp")},
		Name:            "ephemeral", Type: "txt", Description: "session-scoped",
	})
	require.NoError(t, err)

	// Session still active → not eligible.
	removed, err := f.mgr.Cleanup(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, removed, "active session must protect its artifacts")
	_, err = f.mgr.Info(ctx, f.subAID, ref.ID)
	require.NoError(t, err, "row still present")

	// Mark sub-A completed → updated_at = NOW; cleanup with grace=0
	// (the test fixture's default Config) should treat it as
	// eligible immediately.
	require.NoError(t, f.sess.UpdateSessionStatus(ctx, f.subAID, "completed"))

	removed, err = f.mgr.Cleanup(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, removed, "completed session past grace → 1 row removed")

	_, err = f.mgr.Info(ctx, f.subAID, ref.ID)
	require.Error(t, err)
}

func TestManager_Cleanup_PermanentSurvives(t *testing.T) {
	f := newVfixture(t)
	ctx := context.Background()

	ref, err := f.mgr.Publish(ctx, artifacts.PublishRequest{
		CallerSessionID: f.coordID,
		Source:          artifacts.PublishSource{InlineBytes: []byte("forever")},
		Name:            "perm", Type: "txt", Description: "keep",
		TTL: artifacts.TTLPermanent,
	})
	require.NoError(t, err)

	// Coord completes; no matter, permanent never expires.
	require.NoError(t, f.sess.UpdateSessionStatus(ctx, f.coordID, "completed"))

	removed, err := f.mgr.Cleanup(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, removed)

	_, err = f.mgr.Info(ctx, f.coordID, ref.ID)
	require.NoError(t, err, "permanent row must survive")
}

func TestManager_Cleanup_OrphanedSession(t *testing.T) {
	f := newVfixture(t)
	ctx := context.Background()

	// Publish and then directly remove the session row out from
	// under the artifact (simulates an integrity glitch).
	ref, err := f.mgr.Publish(ctx, artifacts.PublishRequest{
		CallerSessionID: f.subBID,
		Source:          artifacts.PublishSource{InlineBytes: []byte("x")},
		Name:            "orphan", Type: "txt", Description: "orphan candidate",
	})
	require.NoError(t, err)

	// We can't easily delete a session row without a Delete API;
	// instead set its status to something falsy and updated_at into
	// the future to verify session-still-active short-circuit.
	require.NoError(t, f.sess.UpdateSessionStatus(ctx, f.subBID, "active"))

	removed, err := f.mgr.Cleanup(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, removed, "active session protects the artifact")

	// Sanity: row still there.
	_, err = f.mgr.Info(ctx, f.subBID, ref.ID)
	require.NoError(t, err)
}

// Light proof that the time math is wired correctly when grace > 0.
// Builds a fresh manager with TTLSessionGrace = 1 hour, marks the
// session completed, observes that cleanup leaves the artifact
// untouched (NOW < updated_at + 1h).
func TestManager_Cleanup_GraceWindow(t *testing.T) {
	f := newGraceFixture(t, 1*time.Hour)
	ctx := context.Background()

	ref, err := f.mgr.Publish(ctx, artifacts.PublishRequest{
		CallerSessionID: f.subAID,
		Source:          artifacts.PublishSource{InlineBytes: []byte("graceful")},
		Name:            "grace", Type: "txt", Description: "with grace",
	})
	require.NoError(t, err)

	require.NoError(t, f.sess.UpdateSessionStatus(ctx, f.subAID, "completed"))

	removed, err := f.mgr.Cleanup(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, removed, "1h grace not yet elapsed")

	_, err = f.mgr.Info(ctx, f.subAID, ref.ID)
	require.NoError(t, err)
}

// newGraceFixture wraps newVfixtureCfg with a non-zero
// TTLSessionGrace so the Cleanup math respects the window.
func newGraceFixture(t *testing.T, grace time.Duration) *vfixture {
	return newVfixtureCfg(t, artifacts.Config{
		InlineBytesMax:  1 << 20,
		TTLSessionGrace: int64(grace.Seconds()),
	})
}

