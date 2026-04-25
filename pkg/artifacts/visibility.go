package artifacts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/artifacts/storage"
	artstore "github.com/hugr-lab/hugen/pkg/artifacts/store"
	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
)

// WidenVisibility raises an artifact's scope and/or grants explicit
// (agent_id, session_id) access. Coordinator-only (FR-008): the
// caller MUST be the coordinator (root session) of the artifact's
// mission graph. Either `vis` or `target` must be set; both may be.
//
// Errors:
//   - ErrUnknownArtifact      — id missing / invisible to caller.
//   - ErrNotCoordinator       — caller has a parent_session_id.
//   - ErrVisibilityNarrowing  — vis is narrower than current.
func (m *Manager) WidenVisibility(ctx context.Context, callerSession, id string, vis Visibility, target *GrantTarget) error {
	if vis == "" && target == nil {
		return fmt.Errorf("artifacts: WidenVisibility: at least one of vis/target required")
	}
	if vis != "" && !vis.IsValid() {
		return ErrInvalidVisibility
	}

	rec, ok, err := m.resolveVisibleArtifact(ctx, callerSession, id)
	if err != nil {
		return fmt.Errorf("artifacts: WidenVisibility: %w", err)
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownArtifact, id)
	}

	// Coordinator gate.
	sess, err := m.deps.SessionEvents.GetSession(ctx, callerSession)
	if err != nil {
		return fmt.Errorf("artifacts: WidenVisibility: lookup session: %w", err)
	}
	if sess == nil {
		return fmt.Errorf("artifacts: WidenVisibility: caller session %q not found", callerSession)
	}
	if sess.ParentSessionID != "" {
		return ErrNotCoordinator
	}

	current := Visibility(rec.Visibility)

	// Visibility step.
	if vis != "" && vis != current {
		if !current.CanWidenTo(vis) {
			return ErrVisibilityNarrowing
		}
		if err := m.store().UpdateVisibility(ctx, id, string(vis)); err != nil {
			return fmt.Errorf("artifacts: WidenVisibility: update: %w", err)
		}
		m.emitVisibilityEvent(ctx, callerSession, id, rec.Name, current, vis)
		current = vis
	}

	// Grant step.
	if target != nil {
		if target.SessionID == "" {
			return fmt.Errorf("artifacts: WidenVisibility: target.SessionID required")
		}
		agentID := target.AgentID
		if agentID == "" {
			agentID = m.deps.AgentID
		}
		if err := m.store().AddGrant(ctx, artstore.GrantRecord{
			ArtifactID: id,
			AgentID:    agentID,
			SessionID:  target.SessionID,
			GrantedBy:  callerSession,
		}); err != nil {
			return fmt.Errorf("artifacts: WidenVisibility: add grant: %w", err)
		}
		m.emitGrantEvent(ctx, callerSession, id, rec.Name, agentID, target.SessionID)
	}

	return nil
}

// ListVisible queries the session_artifacts view, deduplicates by
// id (broadest scope wins), applies tag filtering and the limit, and
// returns the result as []ArtifactRef. Default Limit = 50, max 200.
func (m *Manager) ListVisible(ctx context.Context, callerSession string, filter ListFilter) ([]ArtifactRef, error) {
	if callerSession == "" {
		return nil, fmt.Errorf("artifacts: ListVisible: callerSession required")
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	var rows []artstore.Record
	var err error
	semantic := filter.Search != "" && m.store().EmbedderEnabled() && len(filter.Search) >= 3
	if semantic {
		// Semantic ranking — bypass type/tag filters at the engine
		// level (semantic+filter combinations are an open Hugr
		// question; we apply tag filtering client-side below).
		rows, err = m.store().SessionArtifactsSemantic(ctx, callerSession, filter.Search, limit*4)
	} else {
		rows, err = m.store().SessionArtifacts(ctx, callerSession, artstore.SessionArtifactsFilter{
			Type:  filter.Type,
			Tags:  filter.Tags,
			Limit: limit,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("artifacts: ListVisible: %w", err)
	}
	if semantic {
		// Apply Type/Tags as post-filters on semantic results.
		filtered := make([]artstore.Record, 0, len(rows))
		for _, r := range rows {
			if filter.Type != "" && r.Type != filter.Type {
				continue
			}
			if !tagsContainAll(r.Tags, filter.Tags) {
				continue
			}
			filtered = append(filtered, r)
		}
		rows = filtered
	}

	// Dedup by id, picking the broadest scope (lowest priority order
	// wins per visible_via). The view UNION ALLs so the same artifact
	// may appear once per qualifying scope.
	type best struct {
		rec  artstore.Record
		rank int
	}
	seen := map[string]best{}
	for _, r := range rows {
		rk := scopeRank(r.VisibleVia)
		if cur, ok := seen[r.ID]; !ok || rk > cur.rank {
			seen[r.ID] = best{rec: r, rank: rk}
		}
	}

	out := make([]ArtifactRef, 0, len(seen))
	for _, b := range seen {
		out = append(out, ArtifactRef{
			ID:              b.rec.ID,
			Name:            b.rec.Name,
			Type:            b.rec.Type,
			Visibility:      Visibility(b.rec.Visibility),
			SizeBytes:       b.rec.SizeBytes,
			Tags:            b.rec.Tags,
			CreatedAt:       b.rec.CreatedAt,
			DistanceToQuery: b.rec.DistanceToQuery,
			StorageBackend:  b.rec.StorageBackend,
		})
	}
	// Order by distance ASC when semantic mode active, else
	// created_at DESC. Semantic preserves the engine's ranking
	// (lowest distance first); dedup may have shuffled order.
	if semantic {
		sortRefsByDistanceAsc(out)
	} else {
		sortRefsByCreatedDesc(out)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// tagsContainAll mirrors store.tagsContainAll for the post-filter
// applied on semantic-search rows.
func tagsContainAll(have, want []string) bool {
	if len(want) == 0 {
		return true
	}
	set := map[string]struct{}{}
	for _, t := range have {
		set[t] = struct{}{}
	}
	for _, w := range want {
		if _, ok := set[w]; !ok {
			return false
		}
	}
	return true
}

func sortRefsByDistanceAsc(refs []ArtifactRef) {
	for i := 1; i < len(refs); i++ {
		for j := i; j > 0 && distanceLess(refs[j].DistanceToQuery, refs[j-1].DistanceToQuery); j-- {
			refs[j], refs[j-1] = refs[j-1], refs[j]
		}
	}
}

// distanceLess returns whether a is "smaller" (more similar) than b.
// nil is treated as +Inf so unknown-distance rows sort last.
func distanceLess(a, b *float64) bool {
	switch {
	case a == nil && b == nil:
		return false
	case a == nil:
		return false
	case b == nil:
		return true
	default:
		return *a < *b
	}
}

// Remove removes an artifact (bytes + grants + row) and emits the
// artifact_removed lifecycle event. Caller must be:
//   - the artifact's creator (own session), OR
//   - the coordinator AND the artifact's visibility is "user".
//
// Storage.Delete is idempotent on ErrNotFound (cleanup-friendly).
func (m *Manager) Remove(ctx context.Context, callerSession, id string) error {
	rec, ok, err := m.resolveVisibleArtifact(ctx, callerSession, id)
	if err != nil {
		return fmt.Errorf("artifacts: Remove: %w", err)
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownArtifact, id)
	}

	authorised := rec.SessionID == callerSession
	if !authorised && rec.Visibility == string(VisibilityUser) {
		// Coordinator-on-user-visible: gate on coord status.
		sess, err := m.deps.SessionEvents.GetSession(ctx, callerSession)
		if err != nil {
			return fmt.Errorf("artifacts: Remove: lookup session: %w", err)
		}
		if sess != nil && sess.ParentSessionID == "" {
			authorised = true
		}
	}
	if !authorised {
		return ErrNotAuthorisedToRemove
	}

	// Bytes first — orphaned metadata is worse than orphaned bytes.
	ref := storage.ObjectRef{Backend: rec.StorageBackend, Key: rec.StorageKey}
	if rec.StorageBackend == m.deps.Storage.Name() {
		if err := m.deps.Storage.Delete(ctx, ref); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("artifacts: Remove: delete bytes: %w", err)
		}
	} else {
		// Backend not active — log and continue: the row goes away,
		// the bytes will be reaped when an operator re-attaches the
		// backend's cleanup pass.
		m.log.Warn("artifacts: Remove: bytes left orphaned (backend not active)",
			"artifact_id", id, "backend", rec.StorageBackend)
	}

	if err := m.store().RemoveGrantsByArtifact(ctx, id); err != nil {
		return fmt.Errorf("artifacts: Remove: revoke grants: %w", err)
	}
	if err := m.store().Delete(ctx, id); err != nil {
		return fmt.Errorf("artifacts: Remove: delete row: %w", err)
	}

	m.emitRemovedEvent(ctx, callerSession, id, rec.Name, "manual")
	return nil
}

// Chain returns the derived-from lineage of an artifact, oldest
// ancestor first, current artifact last. Coordinator-only (FR-016)
// — sub-agents calling this get ErrNotCoordinator.
//
// Invisible ancestors are replaced with placeholder ArtifactRefs
// (Name="<hidden>", Visibility=""), preserving the chain length so
// the coordinator sees the depth without leaking metadata. Walks
// up to chainMaxDepth links to bound work on cycles or pathological
// chains.
const chainMaxDepth = 32

func (m *Manager) Chain(ctx context.Context, callerSession, id string) ([]ArtifactRef, error) {
	// Coordinator gate.
	sess, err := m.deps.SessionEvents.GetSession(ctx, callerSession)
	if err != nil {
		return nil, fmt.Errorf("artifacts: Chain: %w", err)
	}
	if sess == nil {
		return nil, fmt.Errorf("artifacts: Chain: caller session %q not found", callerSession)
	}
	if sess.ParentSessionID != "" {
		return nil, ErrNotCoordinator
	}

	// Visibility check on the entry point. If the caller can't see
	// the starting artifact, fail closed.
	rec, ok, err := m.resolveVisibleArtifact(ctx, callerSession, id)
	if err != nil {
		return nil, fmt.Errorf("artifacts: Chain: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownArtifact, id)
	}

	chain := []ArtifactRef{recordToRef(rec)}
	cursor := rec
	for depth := 0; depth < chainMaxDepth; depth++ {
		if cursor.DerivedFrom == "" {
			break
		}
		// Resolve next ancestor through the visibility chokepoint.
		next, visible, err := m.resolveVisibleArtifact(ctx, callerSession, cursor.DerivedFrom)
		if err != nil {
			return nil, fmt.Errorf("artifacts: Chain: walk: %w", err)
		}
		if !visible {
			// Ancestor exists but is hidden from this caller. Emit
			// a placeholder so chain depth is preserved without
			// leaking metadata. Then we have to stop walking — we
			// don't know the hidden ancestor's parent without
			// reading the row, which would leak.
			chain = append([]ArtifactRef{{
				ID:   cursor.DerivedFrom,
				Name: "<hidden>",
			}}, chain...)
			break
		}
		chain = append([]ArtifactRef{recordToRef(next)}, chain...)
		cursor = next
	}
	return chain, nil
}

func recordToRef(rec artstore.Record) ArtifactRef {
	return ArtifactRef{
		ID:             rec.ID,
		Name:           rec.Name,
		Type:           rec.Type,
		Visibility:     Visibility(rec.Visibility),
		SizeBytes:      rec.SizeBytes,
		Tags:           rec.Tags,
		CreatedAt:      rec.CreatedAt,
		StorageBackend: rec.StorageBackend,
	}
}

// scopeRank returns the broadness rank of a visible_via tag. Higher =
// broader. Used by ListVisible to pick the broadest scope when an
// artifact appears in multiple branches of the view.
func scopeRank(via string) int {
	switch via {
	case "self":
		return 0
	case "grant":
		return 1
	case "parent":
		return 2
	case "graph":
		return 3
	case "user":
		return 4
	default:
		return -1
	}
}

func sortRefsByCreatedDesc(refs []ArtifactRef) {
	for i := 1; i < len(refs); i++ {
		for j := i; j > 0 && refs[j].CreatedAt.After(refs[j-1].CreatedAt); j-- {
			refs[j], refs[j-1] = refs[j-1], refs[j]
		}
	}
}

// emitVisibilityEvent records an artifact_granted lifecycle event
// for a visibility-level change. Failure is logged, not surfaced —
// the metadata row is the source of truth.
func (m *Manager) emitVisibilityEvent(ctx context.Context, sessionID, artifactID, name string, oldVis, newVis Visibility) {
	meta := map[string]any{
		"artifact_id":    artifactID,
		"name":           name,
		"old_visibility": string(oldVis),
		"new_visibility": string(newVis),
	}
	if _, err := m.deps.SessionEvents.AppendEventWithSummary(ctx, sessstore.Event{
		SessionID: sessionID,
		AgentID:   m.deps.AgentID,
		EventType: sessstore.EventTypeArtifactGranted,
		Author:    m.deps.AgentID,
		Content:   fmt.Sprintf("widened %s: %s → %s", name, oldVis, newVis),
		Metadata:  meta,
	}, ""); err != nil {
		m.log.Warn("artifacts: emit artifact_granted (vis)", "artifact_id", artifactID, "err", err)
	}
}

// emitGrantEvent records an artifact_granted lifecycle event for a
// (target_agent, target_session) grant.
func (m *Manager) emitGrantEvent(ctx context.Context, sessionID, artifactID, name, targetAgent, targetSession string) {
	meta := map[string]any{
		"artifact_id":       artifactID,
		"name":              name,
		"target_agent_id":   targetAgent,
		"target_session_id": targetSession,
	}
	if _, err := m.deps.SessionEvents.AppendEventWithSummary(ctx, sessstore.Event{
		SessionID: sessionID,
		AgentID:   m.deps.AgentID,
		EventType: sessstore.EventTypeArtifactGranted,
		Author:    m.deps.AgentID,
		Content:   fmt.Sprintf("granted %s to %s/%s", name, targetAgent, targetSession),
		Metadata:  meta,
	}, ""); err != nil {
		m.log.Warn("artifacts: emit artifact_granted (target)", "artifact_id", artifactID, "err", err)
	}
}

// emitRemovedEvent records an artifact_removed lifecycle event with
// the supplied removal reason ("manual" | "ttl_expired").
func (m *Manager) emitRemovedEvent(ctx context.Context, sessionID, artifactID, name, reason string) {
	meta := map[string]any{
		"artifact_id": artifactID,
		"name":        name,
		"reason":      reason,
	}
	metaJSON, _ := json.Marshal(meta)
	metaMap := map[string]any{}
	_ = json.Unmarshal(metaJSON, &metaMap)
	if _, err := m.deps.SessionEvents.AppendEventWithSummary(ctx, sessstore.Event{
		SessionID: sessionID,
		AgentID:   m.deps.AgentID,
		EventType: sessstore.EventTypeArtifactRemoved,
		Author:    m.deps.AgentID,
		Content:   fmt.Sprintf("removed %s (%s)", name, reason),
		Metadata:  metaMap,
	}, ""); err != nil {
		m.log.Warn("artifacts: emit artifact_removed", "artifact_id", artifactID, "err", err)
	}
}
