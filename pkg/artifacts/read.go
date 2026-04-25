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
// mode (admin endpoint). Otherwise the view enforces self / parent /
// graph / grant / user scopes uniformly.
//
// All read paths (OpenReader, Info, WidenVisibility, Remove) funnel
// through this single chokepoint so visibility policy lives in
// exactly one Go function — and one SQL view.
func (m *Manager) resolveVisibleArtifact(ctx context.Context, callerSession, id string) (artstore.Record, bool, error) {
	if id == "" {
		return artstore.Record{}, false, fmt.Errorf("artifacts: resolveVisibleArtifact: empty id")
	}
	return m.store().SessionArtifactByID(ctx, callerSession, id)
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
