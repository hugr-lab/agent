package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/pkg/store/queries"
)

// Record is the typed value the Client exchanges with callers — one
// Go-side mirror of an `artifacts` row. Optional columns use
// pointer / nullable types so "field absent" is distinguishable from
// "field present with zero value".
type Record struct {
	ID                string
	AgentID           string
	Name              string
	Type              string
	StorageKey        string
	StorageBackend    string
	OriginalPath     string
	Description       string
	SessionID         string
	MissionSessionID  string
	DerivedFrom       string
	Visibility        string
	CreatedAt         time.Time
	SizeBytes         int64
	RowCount          *int64
	ColCount          *int
	FileSchema        map[string]any
	Tags              []string
	TTL               string
	// DistanceToQuery is populated only by SessionArtifactsSemantic.
	DistanceToQuery *float64
	// VisibleVia is populated only by SessionArtifacts; one of
	// "self" | "parent" | "graph" | "grant" | "user". Empty on direct
	// reads (Get).
	VisibleVia string
}

// Insert inserts one row into hub.db.agent.artifacts. When the
// Client was wired with EmbedderEnabled=true and Description is
// non-empty, the mutation passes `summary: $description` so Hugr
// embeds server-side and writes `description_embedding` atomically
// with the row.
func (c *Client) Insert(ctx context.Context, r Record) (string, error) {
	if r.ID == "" {
		return "", fmt.Errorf("artifacts/store: Insert requires ID")
	}
	if r.Description == "" {
		return "", fmt.Errorf("artifacts/store: Insert requires Description")
	}
	if r.AgentID == "" {
		r.AgentID = c.agentID
	}
	data := map[string]any{
		"id":              r.ID,
		"agent_id":        r.AgentID,
		"name":            r.Name,
		"type":            r.Type,
		"storage_key":     r.StorageKey,
		"storage_backend": r.StorageBackend,
		"description":     r.Description,
		"session_id":      r.SessionID,
		"visibility":      r.Visibility,
		"ttl":             r.TTL,
	}
	if r.OriginalPath != "" {
		data["original_path"] = r.OriginalPath
	}
	if r.MissionSessionID != "" {
		data["mission_session_id"] = r.MissionSessionID
	}
	if r.DerivedFrom != "" {
		data["derived_from"] = r.DerivedFrom
	}
	if r.SizeBytes > 0 {
		data["size_bytes"] = r.SizeBytes
	}
	if r.RowCount != nil {
		data["row_count"] = *r.RowCount
	}
	if r.ColCount != nil {
		data["col_count"] = *r.ColCount
	}
	if r.FileSchema != nil {
		data["file_schema"] = r.FileSchema
	}
	if len(r.Tags) > 0 {
		data["tags"] = r.Tags
	}

	if c.embedderEnabled {
		if err := queries.RunMutation(ctx, c.querier,
			`mutation ($data: hub_db_artifacts_mut_input_data!, $summary: String!) {
				hub { db { agent {
					insert_artifacts(data: $data, summary: $summary) { id }
				}}}
			}`,
			map[string]any{"data": data, "summary": r.Description},
		); err != nil {
			return "", fmt.Errorf("artifacts/store: insert: %w", err)
		}
		return r.ID, nil
	}
	if err := queries.RunMutation(ctx, c.querier,
		`mutation ($data: hub_db_artifacts_mut_input_data!) {
			hub { db { agent {
				insert_artifacts(data: $data) { id }
			}}}
		}`,
		map[string]any{"data": data},
	); err != nil {
		return "", fmt.Errorf("artifacts/store: insert: %w", err)
	}
	return r.ID, nil
}

// artifactRow mirrors a flat artifacts row as decoded from GraphQL.
// The internal-only intermediate type — callers see Record after
// fromArtifactRow.
type artifactRow struct {
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
}

func (r artifactRow) toRecord() Record {
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
	}
}

const artifactColumns = `id agent_id name type storage_key storage_backend original_path description session_id mission_session_id derived_from visibility created_at size_bytes row_count col_count file_schema tags ttl`

// Get returns the artifact with the given id, scoped to this agent.
// Returns (Record{}, false, nil) when no row matches; an error only
// on transport / engine failure.
func (c *Client) Get(ctx context.Context, id string) (Record, bool, error) {
	if id == "" {
		return Record{}, false, fmt.Errorf("artifacts/store: Get requires id")
	}
	rows, err := queries.RunQuery[[]artifactRow](ctx, c.querier,
		`query ($agent: String!, $id: String!) {
			hub { db { agent {
				artifacts(filter: {agent_id: {eq: $agent}, id: {eq: $id}}) {
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
		return Record{}, false, fmt.Errorf("artifacts/store: get: %w", err)
	}
	if len(rows) == 0 {
		return Record{}, false, nil
	}
	return rows[0].toRecord(), true, nil
}

// ListFilter narrows ListByAgent to a subset of rows. All fields
// are optional; the zero filter returns every artifact owned by
// this agent.
type ListFilter struct {
	SessionID string
	Type      string
	Tags      []string // ALL must be present (AND)
	Limit     int
}

// ListByAgent returns this agent's artifacts honouring filter. NOT
// visibility-aware — callers that need visibility resolution use
// SessionArtifacts (the recursive view). Used by Manager.Cleanup
// (TTL pass) and by tests.
func (c *Client) ListByAgent(ctx context.Context, filter ListFilter) ([]Record, error) {
	args := map[string]any{"agent": c.agentID}
	filters := []string{`agent_id: {eq: $agent}`}
	if filter.SessionID != "" {
		filters = append(filters, `session_id: {eq: $sid}`)
		args["sid"] = filter.SessionID
	}
	if filter.Type != "" {
		filters = append(filters, `type: {eq: $type}`)
		args["type"] = filter.Type
	}
	limitClause := ""
	if filter.Limit > 0 {
		limitClause = fmt.Sprintf(", limit: %d", filter.Limit)
	}
	q := fmt.Sprintf(
		`query (%s) {
			hub { db { agent {
				artifacts(filter: {%s}, order_by: [{field: "created_at", direction: DESC}]%s) {
					%s
				}
			}}}
		}`,
		argSig(args),
		joinAnd(filters),
		limitClause,
		artifactColumns,
	)
	rows, err := queries.RunQuery[[]artifactRow](ctx, c.querier, q, args, "hub.db.agent.artifacts")
	if err != nil {
		if errors.Is(err, types.ErrNoData) || errors.Is(err, types.ErrWrongDataPath) {
			return nil, nil
		}
		return nil, fmt.Errorf("artifacts/store: list: %w", err)
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

// UpdateVisibility sets the visibility column on a single artifact
// row. Strict-widening enforcement happens at the manager layer; the
// store does not police values.
func (c *Client) UpdateVisibility(ctx context.Context, id, visibility string) error {
	if id == "" {
		return fmt.Errorf("artifacts/store: UpdateVisibility requires id")
	}
	return queries.RunMutation(ctx, c.querier,
		`mutation ($id: String!, $data: hub_db_artifacts_mut_data!) {
			hub { db { agent {
				update_artifacts(filter: {id: {eq: $id}}, data: $data) { affected_rows }
			}}}
		}`,
		map[string]any{
			"id":   id,
			"data": map[string]any{"visibility": visibility},
		},
	)
}

// Delete removes an artifact row by id. Idempotent — deleting a
// non-existent row is not an error.
func (c *Client) Delete(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("artifacts/store: Delete requires id")
	}
	return queries.RunMutation(ctx, c.querier,
		`mutation ($id: String!) {
			hub { db { agent {
				delete_artifacts(filter: {id: {eq: $id}}) { affected_rows }
			}}}
		}`,
		map[string]any{"id": id},
	)
}

// joinAnd joins a list of GraphQL filter fragments with comma + space
// for inclusion under a single `filter:` block.
func joinAnd(fragments []string) string {
	out := ""
	for i, f := range fragments {
		if i > 0 {
			out += ", "
		}
		out += f
	}
	return out
}

// argSig produces the GraphQL operation argument signature
// "$agent: String!, $sid: String!, ..." from a vars map. Stable
// ordering is enforced by sorting the map keys.
func argSig(args map[string]any) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	// Sort for determinism.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	out := ""
	for i, k := range keys {
		if i > 0 {
			out += ", "
		}
		out += "$" + k + ": String!"
	}
	return out
}

// tagsContainAll reports whether row carries every tag in want.
// Empty want → true.
func tagsContainAll(row, want []string) bool {
	if len(want) == 0 {
		return true
	}
	have := map[string]struct{}{}
	for _, t := range row {
		have[t] = struct{}{}
	}
	for _, w := range want {
		if _, ok := have[w]; !ok {
			return false
		}
	}
	return true
}
