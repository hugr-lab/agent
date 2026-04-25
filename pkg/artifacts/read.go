package artifacts

import (
	"context"
	"fmt"
	"io"

	"github.com/hugr-lab/hugen/pkg/artifacts/storage"
	artstore "github.com/hugr-lab/hugen/pkg/artifacts/store"
)

// OpenReader resolves the artifact, checks visibility against the
// caller's session, and returns a streaming reader plus Stat from the
// active backend. Empty callerSession switches to user-visibility-only
// mode (the admin endpoint path).
//
// Errors:
//   - ErrUnknownArtifact     — id missing or invisible to caller.
//   - ErrUnregisteredBackend — artifact's recorded backend name does
//     not match the active backend (operator removed it from config).
//   - other storage errors are wrapped verbatim.
func (m *Manager) OpenReader(ctx context.Context, callerSession, id string) (io.ReadCloser, Stat, error) {
	rec, ok, err := m.resolveVisibleArtifact(ctx, callerSession, id)
	if err != nil {
		return nil, Stat{}, fmt.Errorf("artifacts: OpenReader: %w", err)
	}
	if !ok {
		return nil, Stat{}, fmt.Errorf("%w: %s", ErrUnknownArtifact, id)
	}
	if rec.StorageBackend != m.deps.Storage.Name() {
		return nil, Stat{}, fmt.Errorf("%w: %s (artifact backend=%q, active=%q)",
			ErrUnregisteredBackend, id, rec.StorageBackend, m.deps.Storage.Name())
	}
	ref := storage.ObjectRef{Backend: rec.StorageBackend, Key: rec.StorageKey}
	rc, err := m.deps.Storage.Open(ctx, ref)
	if err != nil {
		return nil, Stat{}, fmt.Errorf("artifacts: OpenReader: %w", err)
	}
	st, statErr := m.deps.Storage.Stat(ctx, ref)
	stat := Stat{Size: rec.SizeBytes}
	if statErr == nil {
		stat = Stat{
			Size:        st.Size,
			ModTime:     st.ModTime,
			ContentType: st.ContentType,
		}
	}
	return rc, stat, nil
}

// Info returns the full metadata for an artifact subject to caller
// visibility. Returns ErrUnknownArtifact when the id is missing or
// not visible to callerSession.
func (m *Manager) Info(ctx context.Context, callerSession, id string) (ArtifactDetail, error) {
	rec, ok, err := m.resolveVisibleArtifact(ctx, callerSession, id)
	if err != nil {
		return ArtifactDetail{}, fmt.Errorf("artifacts: Info: %w", err)
	}
	if !ok {
		return ArtifactDetail{}, fmt.Errorf("%w: %s", ErrUnknownArtifact, id)
	}
	return recordToDetail(rec), nil
}

// resolveVisibleArtifact loads an artifact row by id and reports
// whether callerSession can see it. Empty callerSession = user-only
// mode (admin endpoint). Otherwise returns (rec, true, nil) when
// visible by self / user / grant scopes.
//
// Phase-3 US2 implements the four scopes the admin endpoint and ADK
// shims need; parent / graph traversal lands in US4 with the
// `session_artifacts` recursive view (T018). The function is the
// single chokepoint read paths funnel through, so US4 swaps it out
// without touching OpenReader / Info / artifactInfoTool.
func (m *Manager) resolveVisibleArtifact(ctx context.Context, callerSession, id string) (artstore.Record, bool, error) {
	if id == "" {
		return artstore.Record{}, false, fmt.Errorf("artifacts: resolveVisibleArtifact: empty id")
	}
	rec, found, err := m.store().Get(ctx, id)
	if err != nil {
		return artstore.Record{}, false, err
	}
	if !found {
		return artstore.Record{}, false, nil
	}

	if callerSession == "" {
		// Admin endpoint: user-only.
		if rec.Visibility == string(VisibilityUser) {
			rec.VisibleVia = "user"
			return rec, true, nil
		}
		return artstore.Record{}, false, nil
	}

	if rec.Visibility == string(VisibilityUser) {
		rec.VisibleVia = "user"
		return rec, true, nil
	}
	if rec.SessionID == callerSession {
		rec.VisibleVia = "self"
		return rec, true, nil
	}

	// Grant overlay: any explicit row in artifact_grants for the
	// caller's session unlocks the artifact regardless of its scope.
	grants, err := m.store().ListGrantsForSession(ctx, m.deps.AgentID, callerSession)
	if err != nil {
		return artstore.Record{}, false, err
	}
	for _, g := range grants {
		if g.ArtifactID == id {
			rec.VisibleVia = "grant"
			return rec, true, nil
		}
	}

	// parent / graph scopes: require session ancestor traversal that
	// US4's session_artifacts view will provide. Phase-3 US2 returns
	// "invisible" here, which is safe (no false positives) but does
	// hide parent-scope artifacts from coordinator's ADK Load path.
	// The user-facing render path (ListVisible, US4) is the right
	// surface for those reads, so this gap is intentional.
	return artstore.Record{}, false, nil
}

// recordToDetail converts a store.Record into the public
// ArtifactDetail value type returned by Info / artifact_info.
func recordToDetail(rec artstore.Record) ArtifactDetail {
	return ArtifactDetail{
		ArtifactRef: ArtifactRef{
			ID:             rec.ID,
			Name:           rec.Name,
			Type:           rec.Type,
			Visibility:     Visibility(rec.Visibility),
			SizeBytes:      rec.SizeBytes,
			Tags:           rec.Tags,
			CreatedAt:      rec.CreatedAt,
			StorageBackend: rec.StorageBackend,
		},
		Description:      rec.Description,
		OriginalPath:     rec.OriginalPath,
		SessionID:        rec.SessionID,
		MissionSessionID: rec.MissionSessionID,
		DerivedFrom:      rec.DerivedFrom,
		RowCount:         rec.RowCount,
		ColCount:         rec.ColCount,
		FileSchema:       rec.FileSchema,
		TTL:              TTL(rec.TTL),
	}
}
