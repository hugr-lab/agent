// Package store wraps sessstore.Client for mission persistence. A
// mission is a sub-agent session row (sessions.session_type =
// "subagent") carrying skill / role / coord_session_id / depends_on
// in metadata; dependency edges are NOT persisted in a dedicated
// table — the in-memory executor DAG is authoritative during a
// session's lifetime.
//
// Callers outside pkg/missions should hold *Store through the
// graph.Service facade; the sub-package exists so the executor
// can consume a tight API (ListMissions / MarkStatus /
// RecordAbandoned / ListAgentMissions) without the service surface.
package store

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/pkg/missions/graph"
	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
)

// Store is the mission persistence facade.
type Store struct {
	sess    *sessstore.Client
	querier types.Querier
	logger  *slog.Logger
}

// New constructs a Store. Logger is nil-safe.
func New(sess *sessstore.Client, querier types.Querier, logger *slog.Logger) *Store {
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{sess: sess, querier: querier, logger: logger}
}

// AgentID exposes the underlying sessstore client's agent id so
// callers (Executor.RestoreState) can scope lookups by tenant.
func (s *Store) AgentID() string { return s.sess.AgentID() }

// ListMissions returns every mission row whose `parent_session_id ==
// coordSessionID` (i.e. direct children of the coordinator). For
// nested-spawn missions deeper in the tree, use ListAgentMissions
// instead and group by metadata.coord_session_id.
//
// `status` is the persisted column value ("active" | "completed" |
// "failed" | "abandoned"); empty means "no filter".
func (s *Store) ListMissions(ctx context.Context, coordSessionID, status string) ([]graph.MissionRecord, error) {
	children, err := s.sess.ListChildSessions(ctx, coordSessionID)
	if err != nil {
		return nil, fmt.Errorf("missions: list children: %w", err)
	}
	out := make([]graph.MissionRecord, 0, len(children))
	for _, c := range children {
		if c.SessionType != sessstore.SessionTypeSubAgent {
			continue
		}
		if status != "" && c.Status != status {
			continue
		}
		out = append(out, recordFromSession(c, coordSessionID))
	}
	return out, nil
}

// ListAgentMissions returns every active subagent session for this
// agent across coordinators and depth. Used by Executor.RestoreState
// to rebuild every coordinator's DAG in one pass — the caller groups
// by metadata.coord_session_id.
func (s *Store) ListAgentMissions(ctx context.Context) ([]graph.MissionRecord, error) {
	rows, err := s.sess.ListActiveSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("missions: list agent missions: %w", err)
	}
	out := make([]graph.MissionRecord, 0, len(rows))
	for _, r := range rows {
		if r.SessionType != sessstore.SessionTypeSubAgent {
			continue
		}
		coord, _ := r.Metadata[graph.MetadataKeyCoordSession].(string)
		if coord == "" {
			// Pre-spec-007 sub-agents (or rows whose coord pointer was
			// dropped on edit). Fall back to parent_session_id walked
			// once — if the parent is a root, that IS the coord.
			coord = r.ParentSessionID
		}
		out = append(out, recordFromSession(r, coord))
	}
	return out, nil
}

// MarkStatus transitions a mission's persisted status. Maps runtime
// status values (StatusDone / StatusFailed / StatusAbandoned) to
// the persisted column equivalents. Other values pass through
// unchanged so internal callers can write "active".
func (s *Store) MarkStatus(ctx context.Context, missionID, status string) error {
	persisted := runtimeToPersistedStatus(status)
	if err := s.sess.UpdateSessionStatus(ctx, missionID, persisted); err != nil {
		return fmt.Errorf("missions: mark status: %w", err)
	}
	return nil
}

// RecordAbandoned persists a mission row directly in terminal
// abandoned state without ever going through "active". Used by the
// Executor when the cascade-abandon path fires for a mission that
// never got the chance to run (upstream failed before this one was
// promoted). Gives restart + reviewer paths a visible row instead of
// a silent gap in the transcript.
func (s *Store) RecordAbandoned(
	ctx context.Context,
	missionID, parentSessionID, coordSessionID, skill, role, task string,
	dependsOn []string,
	reason string,
) error {
	meta := map[string]any{
		graph.MetadataKeySkill:        skill,
		graph.MetadataKeyRole:         role,
		graph.MetadataKeyCoordSession: coordSessionID,
	}
	if len(dependsOn) > 0 {
		meta[graph.MetadataKeyDependsOn] = dependsOn
	}
	if reason != "" {
		meta["abandon_reason"] = reason
	}
	_, err := s.sess.CreateSession(ctx, sessstore.Record{
		ID:              missionID,
		AgentID:         s.sess.AgentID(),
		ParentSessionID: parentSessionID,
		SessionType:     sessstore.SessionTypeSubAgent,
		Status:          "abandoned",
		Mission:         task,
		Metadata:        meta,
	})
	if err != nil {
		return fmt.Errorf("missions: record abandoned: %w", err)
	}
	return nil
}

// ----------------------------------------------------------------------
// helpers
// ----------------------------------------------------------------------

// recordFromSession converts a persisted sessions row into the
// runtime-shaped MissionRecord the Executor + tools speak.
func recordFromSession(r sessstore.Record, coordSessionID string) graph.MissionRecord {
	skill, _ := r.Metadata[graph.MetadataKeySkill].(string)
	role, _ := r.Metadata[graph.MetadataKeyRole].(string)
	return graph.MissionRecord{
		ID:             r.ID,
		CoordSessionID: coordSessionID,
		Skill:          skill,
		Role:           role,
		Task:           r.Mission,
		Status:         persistedToRuntimeStatus(r.Status),
		DependsOn:      decodeDeps(r.Metadata[graph.MetadataKeyDependsOn]),
		StartedAt:      r.CreatedAt,
		TerminatedAt:   r.UpdatedAt,
	}
}

// decodeDeps coaxes a JSON array out of metadata.depends_on. Hugr
// returns it as []any after JSON decode; we copy into []string.
func decodeDeps(raw any) []string {
	switch v := raw.(type) {
	case nil:
		return nil
	case []string:
		out := make([]string, 0, len(v))
		for _, s := range v {
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// persistedToRuntimeStatus maps the sessions.status column value to
// the Executor's in-memory Status taxonomy. "active" lands as
// StatusRunning because anything alive in hub IS still in flight as
// far as the runtime knows; the Executor's DAG state can demote it
// to StatusReady / StatusPending in memory after the fact.
func persistedToRuntimeStatus(persisted string) string {
	switch persisted {
	case "completed":
		return graph.StatusDone
	case "failed":
		return graph.StatusFailed
	case "abandoned":
		return graph.StatusAbandoned
	case "active":
		return graph.StatusRunning
	default:
		return persisted
	}
}

// runtimeToPersistedStatus is the inverse — runtime statuses to the
// column values UpdateSessionStatus expects.
func runtimeToPersistedStatus(runtime string) string {
	switch runtime {
	case graph.StatusDone:
		return "completed"
	case graph.StatusFailed:
		return "failed"
	case graph.StatusAbandoned:
		return "abandoned"
	default:
		return runtime
	}
}
