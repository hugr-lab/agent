package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/pkg/store/queries"
)

// SessionArtifactsFilter narrows SessionArtifacts to a subset of
// rows. All fields optional; the zero filter returns every artifact
// the calling session can see.
type SessionArtifactsFilter struct {
	// Type filters by artifact type (csv | parquet | …). Optional.
	Type string
	// Tags is the set every returned artifact must carry (AND).
	// Applied client-side after the view returns rows because the
	// schema persists tags as a [String!] column.
	Tags []string
	// Limit caps the number of rows returned. <= 0 → no limit
	// (callers are expected to pass a sane value, e.g. 50).
	Limit int
}

// SessionArtifacts queries the session_artifacts view for the given
// (agentID, sessionID). Returns rows in created_at DESC order with
// `visible_via` populated. Manager.ListVisible deduplicates by id
// (broadest-scope wins); the store layer surfaces every qualifying
// row verbatim to leave that policy in the manager.
//
// Filters: Type and Tags are optional and applied as
// post-conditions on the rows the view returns. Limit is honoured
// server-side via GraphQL.
func (c *Client) SessionArtifacts(ctx context.Context, sessionID string, filter SessionArtifactsFilter) ([]Record, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("artifacts/store: SessionArtifacts requires sessionID")
	}

	// The view returns every artifact column + visible_via. We mirror
	// the artifact field set + visible_via in artifactRowVV.
	args := map[string]any{
		"input": map[string]any{
			"agent_id":   c.agentID,
			"session_id": sessionID,
		},
	}
	limitClause := ""
	if filter.Limit > 0 {
		// 4× headroom for dedup before the manager trims to the
		// caller-requested limit. The view emits one row per
		// qualifying scope per artifact.
		limitClause = fmt.Sprintf(", limit: %d", filter.Limit*4)
	}
	typeClause := ""
	if filter.Type != "" {
		typeClause = `, filter: {type: {eq: $type}}`
		args["type"] = filter.Type
	}

	q := fmt.Sprintf(
		`query ($input: hub_db_session_artifacts_input!%s) {
			hub { db { agent {
				session_artifacts(args: $input%s%s, order_by: [{field: "created_at", direction: DESC}]) {
					%s visible_via
				}
			}}}
		}`,
		typeArgSig(filter.Type),
		typeClause,
		limitClause,
		artifactColumns,
	)

	rows, err := queries.RunQuery[[]artifactRowVV](ctx, c.querier, q, args, "hub.db.agent.session_artifacts")
	if err != nil {
		if errors.Is(err, types.ErrNoData) || errors.Is(err, types.ErrWrongDataPath) {
			return nil, nil
		}
		return nil, fmt.Errorf("artifacts/store: session artifacts: %w", err)
	}

	out := make([]Record, 0, len(rows))
	for _, r := range rows {
		rec := r.toRecord()
		if !tagsContainAll(rec.Tags, filter.Tags) {
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

// SessionArtifactByID looks up a single artifact through the
// session_artifacts view, scoped to the calling session's
// visibility. Returns (rec, false, nil) when invisible / missing —
// the two cases collapse so caller cannot leak existence.
//
// Phase-3: empty callerSession is the "user-visibility-only" mode
// used by the admin endpoint. The view's `user_rows` branch returns
// every user-scoped artifact regardless of session_id, so we route
// the empty-session case through a direct artifacts query instead.
func (c *Client) SessionArtifactByID(ctx context.Context, sessionID, id string) (Record, bool, error) {
	if id == "" {
		return Record{}, false, fmt.Errorf("artifacts/store: SessionArtifactByID requires id")
	}
	if sessionID == "" {
		// Admin endpoint path: filter the artifacts table to
		// user-visibility only.
		rows, err := queries.RunQuery[[]artifactRow](ctx, c.querier,
			`query ($agent: String!, $id: String!) {
				hub { db { agent {
					artifacts(filter: {agent_id: {eq: $agent}, id: {eq: $id}, visibility: {eq: "user"}}) {
						`+artifactColumns+`
					}
				}}}
			}`,
			map[string]any{"agent": c.agentID, "id": id},
			"hub.db.agent.artifacts",
		)
		if err != nil {
			if errors.Is(err, types.ErrNoData) || errors.Is(err, types.ErrWrongDataPath) {
				return Record{}, false, nil
			}
			return Record{}, false, fmt.Errorf("artifacts/store: session artifact by id (admin): %w", err)
		}
		if len(rows) == 0 {
			return Record{}, false, nil
		}
		rec := rows[0].toRecord()
		rec.VisibleVia = "user"
		return rec, true, nil
	}

	rows, err := queries.RunQuery[[]artifactRowVV](ctx, c.querier,
		`query ($input: hub_db_session_artifacts_input!, $id: String!) {
			hub { db { agent {
				session_artifacts(args: $input, filter: {id: {eq: $id}}, order_by: [{field: "visible_via", direction: ASC}], limit: 1) {
					`+artifactColumns+` visible_via
				}
			}}}
		}`,
		map[string]any{
			"input": map[string]any{
				"agent_id":   c.agentID,
				"session_id": sessionID,
			},
			"id": id,
		},
		"hub.db.agent.session_artifacts",
	)
	if err != nil {
		if errors.Is(err, types.ErrNoData) || errors.Is(err, types.ErrWrongDataPath) {
			return Record{}, false, nil
		}
		return Record{}, false, fmt.Errorf("artifacts/store: session artifact by id: %w", err)
	}
	if len(rows) == 0 {
		return Record{}, false, nil
	}
	return rows[0].toRecord(), true, nil
}

// artifactRowVV is artifactRow + visible_via. Kept private so the
// manager-facing Record type stays the single API surface. Fields
// duplicated explicitly (rather than embedding artifactRow) because
// the GraphQL JSON decoder used by RunQuery handles flat field maps
// reliably; embedded-struct field promotion has bitten us before.
type artifactRowVV struct {
	ID               string         `json:"id"`
	AgentID          string         `json:"agent_id"`
	Name             string         `json:"name"`
	Type             string         `json:"type"`
	StorageKey       string         `json:"storage_key"`
	StorageBackend   string         `json:"storage_backend"`
	OriginalPath     string         `json:"original_path"`
	Description      string         `json:"description"`
	SessionID        string         `json:"session_id"`
	MissionSessionID string         `json:"mission_session_id"`
	DerivedFrom      string         `json:"derived_from"`
	Visibility       string         `json:"visibility"`
	CreatedAt        time.Time      `json:"created_at"`
	SizeBytes        int64          `json:"size_bytes"`
	RowCount         *int64         `json:"row_count"`
	ColCount         *int           `json:"col_count"`
	FileSchema       map[string]any `json:"file_schema"`
	Tags             []string       `json:"tags"`
	TTL              string         `json:"ttl"`
	VisibleVia       string         `json:"visible_via"`
}

func (r artifactRowVV) toRecord() Record {
	return Record{
		ID:               r.ID,
		AgentID:          r.AgentID,
		Name:             r.Name,
		Type:             r.Type,
		StorageKey:       r.StorageKey,
		StorageBackend:   r.StorageBackend,
		OriginalPath:     r.OriginalPath,
		Description:      r.Description,
		SessionID:        r.SessionID,
		MissionSessionID: r.MissionSessionID,
		DerivedFrom:      r.DerivedFrom,
		Visibility:       r.Visibility,
		CreatedAt:        r.CreatedAt,
		SizeBytes:        r.SizeBytes,
		RowCount:         r.RowCount,
		ColCount:         r.ColCount,
		FileSchema:       r.FileSchema,
		Tags:             r.Tags,
		TTL:              r.TTL,
		VisibleVia:       r.VisibleVia,
	}
}

// typeArgSig returns ", $type: String!" when typeArg is non-empty,
// "" otherwise. Used to extend SessionArtifacts' GraphQL operation
// signature only when the type filter is in play.
func typeArgSig(typeArg string) string {
	if typeArg == "" {
		return ""
	}
	return ", $type: String!"
}
