package search

import (
	"context"
	"errors"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/store/queries"
	"github.com/hugr-lab/query-engine/types"
)

// CorpusSpec is the resolved set of session_ids the search query
// should run against, plus a hint for which GraphQL shape to use.
type CorpusSpec struct {
	// SessionIDs is the set of sessions to search across.
	SessionIDs []string

	// UseChainView is true when the corpus should run against the
	// `session_events_chain` recursive view instead of the
	// `session_events` table directly. Currently always false —
	// foundation uses the simpler IN-clause approach over
	// session_events. Reserved for future deepening of multi-level
	// mission graphs.
	UseChainView bool
}

// resolveCorpus returns the session IDs the search query should
// target for a given (scope, callerSessionID) pair.
//
//   - scope=turn / mission → singleton with the caller's own id.
//   - scope=session → coordinator root + every direct sub-agent
//     session whose parent_session_id points at it (one level
//     deep — the typical mission topology). Multi-level
//     descendants follow in a phase-4.5 enhancement once we have
//     scenarios deeper than 1.
//   - scope=user → every root session belonging to the same
//     owner_id, optionally clamped by date window.
func (s *Service) resolveCorpus(ctx context.Context, scope Scope, callerSessionID string, dateFrom, dateTo string) (CorpusSpec, error) {
	switch scope {
	case ScopeTurn, ScopeMission:
		if callerSessionID == "" {
			return CorpusSpec{}, ErrUnknownSession
		}
		return CorpusSpec{SessionIDs: []string{callerSessionID}}, nil

	case ScopeSession:
		// Coord-only. Caller is expected to be a root session;
		// resolve the coord root either directly from the
		// caller's session_type or by walking parent_session_id.
		coordID, err := s.resolveCoordRoot(ctx, callerSessionID)
		if err != nil {
			return CorpusSpec{}, err
		}
		ids, err := s.collectChildSessions(ctx, coordID)
		if err != nil {
			return CorpusSpec{}, err
		}
		// Always include the coord root itself — its events are
		// the user-coord conversation.
		out := append([]string{coordID}, ids...)
		return CorpusSpec{SessionIDs: out}, nil

	case ScopeUser:
		ownerID, err := s.resolveOwnerID(ctx, callerSessionID)
		if err != nil {
			return CorpusSpec{}, err
		}
		ids, err := s.collectRootsForOwner(ctx, ownerID, dateFrom, dateTo)
		if err != nil {
			return CorpusSpec{}, err
		}
		return CorpusSpec{SessionIDs: ids}, nil

	default:
		return CorpusSpec{}, fmt.Errorf("%w: %q", ErrInvalidScope, scope)
	}
}

// resolveCoordRoot returns the root coordinator session id for the
// given caller session. If caller is already a root, returns its
// own id. Otherwise walks parent_session_id once (the typical
// 1-level mission topology); deeper walks are tracked as a
// phase-4.5 enhancement.
func (s *Service) resolveCoordRoot(ctx context.Context, callerSessionID string) (string, error) {
	rec, err := s.sessR.GetSession(ctx, callerSessionID)
	if err != nil {
		return "", fmt.Errorf("search: resolve coord root: %w", err)
	}
	if rec == nil {
		return "", ErrUnknownSession
	}
	if rec.SessionType == "root" || rec.ParentSessionID == "" {
		return callerSessionID, nil
	}
	// Walk up — typically just one hop for sub-agent → root.
	cur := rec.ParentSessionID
	for i := 0; i < 8; i++ {
		parent, err := s.sessR.GetSession(ctx, cur)
		if err != nil || parent == nil {
			return cur, nil
		}
		if parent.SessionType == "root" || parent.ParentSessionID == "" {
			return cur, nil
		}
		cur = parent.ParentSessionID
	}
	return cur, nil
}

// resolveOwnerID extracts the owner_id of the caller's root session.
// Falls back to "" when the field is missing — the user-scope
// corpus collector treats empty owner_id as "no roots match".
func (s *Service) resolveOwnerID(ctx context.Context, callerSessionID string) (string, error) {
	coordID, err := s.resolveCoordRoot(ctx, callerSessionID)
	if err != nil {
		return "", err
	}
	rec, err := s.sessR.GetSession(ctx, coordID)
	if err != nil {
		return "", fmt.Errorf("search: resolve owner: %w", err)
	}
	if rec == nil {
		return "", ErrUnknownSession
	}
	return rec.OwnerID, nil
}

// collectChildSessions returns every session whose
// parent_session_id == coordID. One-level walk only — most missions
// are 1 deep; multi-level extension is tracked.
func (s *Service) collectChildSessions(ctx context.Context, coordID string) ([]string, error) {
	type row struct {
		ID string `json:"id"`
	}
	rows, err := queries.RunQuery[[]row](ctx, s.q,
		`query ($agent: String!, $coord: String!) {
			hub { db { agent {
				sessions(filter: {
					agent_id: {eq: $agent}
					parent_session_id: {eq: $coord}
				}, limit: 1000) {
					id
				}
			}}}
		}`,
		map[string]any{"agent": s.agentID, "coord": coordID},
		"hub.db.agent.sessions",
	)
	if err != nil {
		if errors.Is(err, types.ErrNoData) || errors.Is(err, types.ErrWrongDataPath) {
			return nil, nil
		}
		return nil, fmt.Errorf("search: collect children: %w", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out, nil
}

// collectRootsForOwner returns every root session id belonging to
// the given owner_id. Optional date_from / date_to clamp by
// started_at. Limit hardcoded to 200 — the typical user has far
// fewer conversations than that; if more matches exist, only the
// 200 most recent are searched.
func (s *Service) collectRootsForOwner(ctx context.Context, ownerID, dateFrom, dateTo string) ([]string, error) {
	if ownerID == "" {
		return nil, nil
	}
	type row struct {
		ID string `json:"id"`
	}
	filter := map[string]any{
		"agent_id":     map[string]any{"eq": s.agentID},
		"owner_id":     map[string]any{"eq": ownerID},
		"session_type": map[string]any{"eq": "root"},
	}
	if dateFrom != "" || dateTo != "" {
		started := map[string]any{}
		if dateFrom != "" {
			started["gte"] = dateFrom
		}
		if dateTo != "" {
			started["lte"] = dateTo
		}
		filter["started_at"] = started
	}
	rows, err := queries.RunQuery[[]row](ctx, s.q,
		`query ($filter: hub_db_sessions_filter, $limit: Int!) {
			hub { db { agent {
				sessions(
					filter: $filter
					order_by: [{field: "started_at", direction: DESC}]
					limit: $limit
				) { id }
			}}}
		}`,
		map[string]any{"filter": filter, "limit": 200},
		"hub.db.agent.sessions",
	)
	if err != nil {
		if errors.Is(err, types.ErrNoData) || errors.Is(err, types.ErrWrongDataPath) {
			return nil, nil
		}
		return nil, fmt.Errorf("search: collect roots: %w", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out, nil
}
