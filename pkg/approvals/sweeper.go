package approvals

import (
	"context"
	"fmt"

	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
)

// SweepExpired runs a single bulk transition: every pending row
// older than cfg.DefaultTimeout becomes `expired`. For each affected
// row, emits one approval_responded event on the row's coord
// session with decision=expired, author=system.
//
// Wired as a pkg/scheduler.Cron task at cfg.SweeperInterval (default
// every 5 min). Returns the count of expired rows.
func (m *Manager) SweepExpired(ctx context.Context) (int, error) {
	now := m.nowFn()
	cutoff := now.Add(-m.cfg.DefaultTimeout)
	rows, err := m.store.SweepExpired(ctx, cutoff)
	if err != nil {
		return 0, fmt.Errorf("approvals: sweep: %w", err)
	}

	for _, r := range rows {
		// Emit approval_responded for each expired row so the
		// coordinator can surface "approval timed out" to the user
		// and the audit log records the transition.
		if _, err := m.events.AppendEvent(ctx, sessstore.Event{
			SessionID: r.CoordSessionID,
			AgentID:   r.AgentID,
			EventType: sessstore.EventTypeApprovalResponded,
			Author:    "system",
			Content:   fmt.Sprintf("Approval %s expired", r.ID),
			Metadata: map[string]any{
				"approval_id": r.ID,
				"decision":    "expired",
			},
		}); err != nil {
			m.logger.WarnContext(ctx, "approvals: emit expired event",
				"id", r.ID, "err", err)
		}

		// Cancel the gated mission so observers see the terminal
		// state — the user never responded in time.
		if m.missions != nil {
			if err := m.missions.MarkStatus(ctx, r.MissionSessionID, "cancelled"); err != nil {
				m.logger.WarnContext(ctx, "approvals: cancel mission on expire",
					"approval", r.ID, "mission", r.MissionSessionID, "err", err)
			}
		}
	}

	return len(rows), nil
}
