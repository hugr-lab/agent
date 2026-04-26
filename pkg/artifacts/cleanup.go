package artifacts

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hugr-lab/hugen/pkg/artifacts/storage"
	artstore "github.com/hugr-lab/hugen/pkg/artifacts/store"
)

// Cleanup expires artifacts whose TTL has elapsed. Iterates the
// agent's non-permanent rows, removes bytes via Storage.Delete
// (idempotent on ErrNotFound), revokes grants, deletes the row, and
// emits artifact_removed with reason="ttl_expired" on the creator's
// session if it still exists.
//
// Eligibility per artifact:
//
//   - ttl = 'permanent'   → never (filtered out at the store level).
//   - ttl = '7d'          → created_at < NOW() - cfg.TTL7dSeconds.
//   - ttl = '30d'         → created_at < NOW() - cfg.TTL30dSeconds.
//   - ttl = 'session'     → creator session.status = 'completed' AND
//                           session.updated_at < NOW() - cfg.TTLSessionGrace.
//
// Returns the count of artifacts removed and the first transient
// error encountered (per-artifact errors are logged and the loop
// continues — one bad row should not block the cron).
func (m *Manager) Cleanup(ctx context.Context) (int, error) {
	now := time.Now().UTC()

	// Pull every non-permanent row for this agent. At phase-3 scale
	// (≤ 10K rows / agent) this is one round-trip; if cardinality
	// outgrows that we revisit with paginated cursors.
	candidates, err := m.fetchCleanupCandidates(ctx)
	if err != nil {
		return 0, fmt.Errorf("artifacts: Cleanup: list candidates: %w", err)
	}

	removed := 0
	for _, c := range candidates {
		eligible, err := m.eligibleForCleanup(ctx, c, now)
		if err != nil {
			m.log.Warn("artifacts: Cleanup: eligibility check", "artifact_id", c.ID, "err", err)
			continue
		}
		if !eligible {
			continue
		}
		if err := m.expireOne(ctx, c); err != nil {
			m.log.Warn("artifacts: Cleanup: expire", "artifact_id", c.ID, "err", err)
			continue
		}
		removed++
	}
	return removed, nil
}

// fetchCleanupCandidates returns the agent's artifacts whose TTL
// class is in the cleanup-eligible set ('session' | '7d' | '30d').
// Permanent rows are skipped here so eligibleForCleanup never sees
// them. One round-trip; phase-3 scale (≤ 10K rows / agent) doesn't
// warrant pagination.
func (m *Manager) fetchCleanupCandidates(ctx context.Context) ([]artstore.Record, error) {
	rows, err := m.store().ListByAgent(ctx, artstore.ListFilter{})
	if err != nil {
		return nil, err
	}
	out := make([]artstore.Record, 0, len(rows))
	for _, r := range rows {
		switch r.TTL {
		case string(TTLSession), string(TTL7d), string(TTL30d):
			out = append(out, r)
		}
	}
	return out, nil
}

// eligibleForCleanup applies the per-class deadline check. Returns
// (true, nil) when the artifact is past its eligibility threshold.
//
// Timezone caveat: the engine returns `created_at` as a TIMESTAMPTZ
// originating from CURRENT_TIMESTAMP / NOW(); on hosts whose system
// clock isn't UTC, the value may be wall-clock-local but tagged
// `+0000` by the JSON serializer. Production deployments run in
// UTC containers where this collapses; for local development with
// a non-UTC clock the comparison is shifted by the host offset
// (artifact "appears" to be in the future), so a fresh artifact
// won't be reaped until `(host_offset_seconds + threshold)` have
// elapsed — never the other way around. We accept this for phase 3.
func (m *Manager) eligibleForCleanup(ctx context.Context, rec artstore.Record, now time.Time) (bool, error) {
	switch rec.TTL {
	case string(TTLPermanent):
		return false, nil

	case string(TTL7d):
		threshold := int64(7 * 24 * 60 * 60)
		if m.cfg.TTL7dSeconds > 0 {
			threshold = m.cfg.TTL7dSeconds
		}
		return rec.CreatedAt.Add(time.Duration(threshold) * time.Second).Before(now), nil

	case string(TTL30d):
		threshold := int64(30 * 24 * 60 * 60)
		if m.cfg.TTL30dSeconds > 0 {
			threshold = m.cfg.TTL30dSeconds
		}
		return rec.CreatedAt.Add(time.Duration(threshold) * time.Second).Before(now), nil

	case string(TTLSession):
		sess, err := m.deps.SessionEvents.GetSession(ctx, rec.SessionID)
		if err != nil {
			return false, err
		}
		if sess == nil {
			// Creator session was deleted out from under us — treat
			// as eligible: the row is orphaned.
			return true, nil
		}
		if sess.Status != "completed" {
			return false, nil
		}
		grace := m.cfg.TTLSessionGrace
		if grace <= 0 {
			// Grace disabled → eligible the moment the session
			// reaches completed. Avoids depending on the engine's
			// updated_at refresh semantics for the default path.
			return true, nil
		}
		// With a non-zero grace, the artifact's own created_at is
		// the floor: we want "the producer has been done long
		// enough that the artifact's been around past the grace".
		// Using created_at instead of session.updated_at keeps
		// behaviour consistent across DuckDB / Postgres timestamp
		// quirks.
		return rec.CreatedAt.Add(time.Duration(grace) * time.Second).Before(now), nil

	default:
		return false, nil
	}
}

// expireOne removes one artifact's bytes + grants + row and emits
// the lifecycle event. Idempotent on a missing storage object.
func (m *Manager) expireOne(ctx context.Context, rec artstore.Record) error {
	if rec.StorageBackend == m.deps.Storage.Name() {
		ref := storage.ObjectRef{Backend: rec.StorageBackend, Key: rec.StorageKey}
		if err := m.deps.Storage.Delete(ctx, ref); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("storage delete: %w", err)
		}
	} else {
		m.log.Warn("artifacts: Cleanup: bytes left orphaned (backend not active)",
			"artifact_id", rec.ID, "backend", rec.StorageBackend)
	}
	if err := m.store().RemoveGrantsByArtifact(ctx, rec.ID); err != nil {
		return fmt.Errorf("revoke grants: %w", err)
	}
	if err := m.store().Delete(ctx, rec.ID); err != nil {
		return fmt.Errorf("delete row: %w", err)
	}
	m.emitRemovedEvent(ctx, rec.SessionID, rec.ID, rec.Name, "ttl_expired")
	return nil
}
