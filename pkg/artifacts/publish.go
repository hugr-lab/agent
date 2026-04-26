package artifacts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hugr-lab/hugen/pkg/artifacts/storage"
	artstore "github.com/hugr-lab/hugen/pkg/artifacts/store"
	"github.com/hugr-lab/hugen/pkg/id"
	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
)

// Publish writes the source bytes through the active storage
// backend, inserts an artifacts row (with server-side description
// embedding via Hugr `summary:`), and emits an artifact_published
// event on the caller's session. Returns the new ArtifactRef.
func (m *Manager) Publish(ctx context.Context, req PublishRequest) (ArtifactRef, error) {
	if err := validatePublishRequest(req, m.cfg.InlineBytesMax); err != nil {
		return ArtifactRef{}, err
	}
	if req.Visibility == "" {
		req.Visibility = VisibilitySelf
	}
	if req.TTL == "" {
		req.TTL = TTLSession
	}

	src, sizeHint, sourcePath, err := openSource(req.Source)
	if err != nil {
		return ArtifactRef{}, err
	}
	defer src.Close()

	artifactID := id.New(id.PrefixArtifact, m.deps.AgentShort)
	hint := storage.PutHint{
		ID:       artifactID,
		Name:     req.Name,
		Type:     strings.ToLower(req.Type),
		SizeHint: sizeHint,
	}
	ref, err := m.deps.Storage.Put(ctx, hint, src)
	if err != nil {
		return ArtifactRef{}, fmt.Errorf("artifacts: Publish: storage put: %w", err)
	}

	// Stat-after-Put resolves the authoritative size + content type.
	// Failure here is non-fatal — the row still persists with the
	// caller-provided sizeHint.
	stat, statErr := m.deps.Storage.Stat(ctx, ref)
	size := sizeHint
	if statErr == nil && stat.Size > 0 {
		size = stat.Size
	}

	// missionSessionID: equals session_id only when the caller is a
	// sub-agent session. We don't have per-session metadata here
	// (sessions.Manager owns that); for the foundation slice we set
	// it equal to caller session id and let read paths discriminate
	// via session_type. The Manager dep set may grow a session-meta
	// resolver in a later slice.
	missionSession := req.CallerSessionID

	rec := artstore.Record{
		ID:               artifactID,
		AgentID:          m.deps.AgentID,
		Name:             strings.TrimSpace(req.Name),
		Type:             hint.Type,
		StorageKey:       ref.Key,
		StorageBackend:   ref.Backend,
		OriginalPath:     sourcePath,
		Description:      strings.TrimSpace(req.Description),
		SessionID:        req.CallerSessionID,
		MissionSessionID: missionSession,
		DerivedFrom:      strings.TrimSpace(req.DerivedFrom),
		Visibility:       string(req.Visibility),
		SizeBytes:        size,
		Tags:             req.Tags,
		TTL:              string(req.TTL),
	}
	if _, err := m.store().Insert(ctx, rec); err != nil {
		// Best-effort cleanup of the bytes when the metadata insert
		// fails — orphaned objects on the backend are harmless but
		// confusing. Errors during cleanup are logged, not surfaced.
		if cleanupErr := m.deps.Storage.Delete(ctx, ref); cleanupErr != nil && !errors.Is(cleanupErr, storage.ErrNotFound) {
			m.log.Warn("artifacts: orphan cleanup after insert failure",
				"artifact_id", artifactID, "err", cleanupErr)
		}
		return ArtifactRef{}, fmt.Errorf("artifacts: Publish: insert row: %w", err)
	}

	// Emit lifecycle event on the creator's session. Failure is
	// logged but does not roll back the publish — the metadata row
	// is the source of truth; events are an audit trail.
	meta := sessstore.ArtifactPublishedMeta{
		ArtifactID: artifactID,
		Name:       rec.Name,
		Type:       rec.Type,
		Visibility: rec.Visibility,
		SizeBytes:  size,
		Tags:       rec.Tags,
		Source:     req.EventSource,
	}
	metaJSON, _ := json.Marshal(meta)
	metaMap := map[string]any{}
	_ = json.Unmarshal(metaJSON, &metaMap)
	if _, err := m.deps.SessionEvents.AppendEventWithSummary(ctx, sessstore.Event{
		SessionID: req.CallerSessionID,
		AgentID:   m.deps.AgentID,
		EventType: sessstore.EventTypeArtifactPublished,
		Author:    m.deps.AgentID,
		Content:   rec.Description,
		Metadata:  metaMap,
	}, ""); err != nil {
		m.log.Warn("artifacts: emit artifact_published event",
			"artifact_id", artifactID, "session_id", req.CallerSessionID, "err", err)
	}

	return ArtifactRef{
		ID:             artifactID,
		Name:           rec.Name,
		Type:           rec.Type,
		Visibility:     Visibility(rec.Visibility),
		SizeBytes:      size,
		Tags:           rec.Tags,
		CreatedAt:      time.Now().UTC(),
		StorageBackend: ref.Backend,
	}, nil
}

// AddGrant inserts an artifact_grants row authorising (agentID,
// sessionID) to see artifactID. Idempotent on duplicate. Issued by
// the coordinator (via WidenVisibility) and by the missions
// executor (via MissionSpec.InputArtifacts at spawn time).
func (m *Manager) AddGrant(ctx context.Context, artifactID, agentID, sessionID, grantedBy string) error {
	if artifactID == "" || sessionID == "" || grantedBy == "" {
		return fmt.Errorf("artifacts: AddGrant: missing required arg")
	}
	if agentID == "" {
		agentID = m.deps.AgentID
	}
	// Verify the artifact exists in this agent's scope before
	// recording the grant — invisible / unknown ids must not produce
	// orphan rows.
	if _, found, err := m.store().Get(ctx, artifactID); err != nil {
		return fmt.Errorf("artifacts: AddGrant: lookup: %w", err)
	} else if !found {
		return fmt.Errorf("artifacts: AddGrant: %w: %s", ErrUnknownArtifact, artifactID)
	}
	return m.store().AddGrant(ctx, artstore.GrantRecord{
		ArtifactID: artifactID,
		AgentID:    agentID,
		SessionID:  sessionID,
		GrantedBy:  grantedBy,
	})
}

// store lazily instantiates the artifacts store client. Manager
// constructor doesn't take it directly because the same Querier the
// runtime threads to memory/sessions can build it inline; a single
// Client per Manager keeps memoised references contained.
func (m *Manager) store() *artstore.Client {
	if m.cachedStore != nil {
		return m.cachedStore
	}
	c, err := artstore.New(m.deps.Querier, artstore.Options{
		AgentID:         m.deps.AgentID,
		AgentShort:      m.deps.AgentShort,
		Logger:          m.log,
		EmbedderEnabled: m.deps.EmbedderEnabled,
	})
	if err != nil {
		// Manager.New validated the querier already; this is a
		// programmer-error path. Log and return a partial client
		// would mask issues — panic.
		panic(fmt.Errorf("artifacts: store: %w", err))
	}
	m.cachedStore = c
	return c
}

// validatePublishRequest enforces the manager-side constraints
// before any I/O happens. Returns one of the package's sentinel
// errors so callers can distinguish input errors from runtime
// failures.
func validatePublishRequest(req PublishRequest, inlineMax int64) error {
	if req.CallerSessionID == "" {
		return fmt.Errorf("artifacts: Publish: CallerSessionID required")
	}
	if strings.TrimSpace(req.Name) == "" {
		return fmt.Errorf("artifacts: Publish: Name required")
	}
	if strings.TrimSpace(req.Type) == "" {
		return fmt.Errorf("artifacts: Publish: Type required")
	}
	if strings.TrimSpace(req.Description) == "" {
		return ErrDescriptionRequired
	}
	if req.Source.HasPath() == req.Source.HasInline() {
		// True when both are set OR both are empty.
		return ErrSourceAmbiguous
	}
	if req.Source.HasInline() && inlineMax > 0 && int64(len(req.Source.InlineBytes)) > inlineMax {
		return ErrInlineBytesTooLarge
	}
	if req.Visibility != "" && !req.Visibility.IsValid() {
		return ErrInvalidVisibility
	}
	if req.TTL != "" && !req.TTL.IsValid() {
		return ErrInvalidTTL
	}
	return nil
}

// openSource opens the publish source as an io.ReadCloser. Returns
// the source size hint (-1 when unknown) and the optional path the
// caller supplied (recorded as artifacts.original_path for audit).
func openSource(src PublishSource) (io.ReadCloser, int64, string, error) {
	switch {
	case src.HasPath():
		// Resolve any path traversal up front.
		abs, err := filepath.Abs(src.Path)
		if err != nil {
			return nil, 0, "", fmt.Errorf("artifacts: resolve path %q: %w", src.Path, err)
		}
		f, err := os.Open(abs)
		if err != nil {
			return nil, 0, "", fmt.Errorf("artifacts: open source %q: %w", abs, err)
		}
		stat, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, 0, "", fmt.Errorf("artifacts: stat source %q: %w", abs, err)
		}
		return f, stat.Size(), abs, nil
	case src.HasInline():
		buf := bytes.NewReader(src.InlineBytes)
		return nopCloser{Reader: buf}, int64(len(src.InlineBytes)), "", nil
	default:
		return nil, 0, "", ErrSourceAmbiguous
	}
}
