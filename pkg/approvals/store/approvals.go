package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/pkg/store/queries"
)

// ApprovalRecord is the typed mirror of one approvals row exchanged
// across the store boundary. Args + Response are JSON-encoded
// strings on the wire; the store decodes/encodes for callers.
type ApprovalRecord struct {
	ID               string
	AgentID          string
	MissionSessionID string
	CoordSessionID   string
	ToolName         string
	Args             map[string]any
	Risk             string
	Status           string
	Response         map[string]any // nil when status=pending
	CreatedAt        time.Time
	RespondedAt      *time.Time
}

// approvalRow mirrors the GraphQL projection. JSON columns arrive
// as already-decoded maps via the engine's ScanData path.
type approvalRow struct {
	ID               string         `json:"id"`
	AgentID          string         `json:"agent_id"`
	MissionSessionID string         `json:"mission_session_id"`
	CoordSessionID   string         `json:"coord_session_id"`
	ToolName         string         `json:"tool_name"`
	Args             map[string]any `json:"args"`
	Risk             string         `json:"risk"`
	Status           string         `json:"status"`
	Response         map[string]any `json:"response"`
	CreatedAt        time.Time      `json:"created_at"`
	RespondedAt      *time.Time     `json:"responded_at,omitempty"`
}

func (r approvalRow) toRecord() ApprovalRecord {
	return ApprovalRecord{
		ID:               r.ID,
		AgentID:          r.AgentID,
		MissionSessionID: r.MissionSessionID,
		CoordSessionID:   r.CoordSessionID,
		ToolName:         r.ToolName,
		Args:             r.Args,
		Risk:             r.Risk,
		Status:           r.Status,
		Response:         r.Response,
		CreatedAt:        r.CreatedAt,
		RespondedAt:      r.RespondedAt,
	}
}

// InsertApproval inserts one pending row.
func (c *Client) InsertApproval(ctx context.Context, r ApprovalRecord) error {
	if r.ID == "" {
		return fmt.Errorf("approvals/store: InsertApproval requires ID")
	}
	if r.AgentID == "" {
		r.AgentID = c.agentID
	}
	data := map[string]any{
		"id":                 r.ID,
		"agent_id":           r.AgentID,
		"mission_session_id": r.MissionSessionID,
		"coord_session_id":   r.CoordSessionID,
		"tool_name":          r.ToolName,
		"args":               r.Args,
		"risk":               r.Risk,
		"status":             r.Status,
	}
	return queries.RunMutation(ctx, c.querier,
		`mutation ($data: hub_db_approvals_mut_input_data!) {
			hub { db { agent {
				insert_approvals(data: $data) { id }
			}}}
		}`,
		map[string]any{"data": data},
	)
}

// GetApproval reads one row by id. Returns ErrApprovalNotFound when
// the row does not exist.
func (c *Client) GetApproval(ctx context.Context, id string) (ApprovalRecord, error) {
	if id == "" {
		return ApprovalRecord{}, fmt.Errorf("approvals/store: GetApproval requires id")
	}
	rows, err := queries.RunQuery[[]approvalRow](ctx, c.querier,
		`query ($id: String!) {
			hub { db { agent {
				approvals(filter: {id: {eq: $id}}, limit: 1) {
					id agent_id mission_session_id coord_session_id
					tool_name args risk status response
					created_at responded_at
				}
			}}}
		}`,
		map[string]any{"id": id},
		"hub.db.agent.approvals",
	)
	if err != nil {
		if errors.Is(err, types.ErrNoData) || errors.Is(err, types.ErrWrongDataPath) {
			return ApprovalRecord{}, ErrApprovalNotFound
		}
		return ApprovalRecord{}, fmt.Errorf("approvals/store: get approval: %w", err)
	}
	if len(rows) == 0 {
		return ApprovalRecord{}, ErrApprovalNotFound
	}
	return rows[0].toRecord(), nil
}

// ListApprovals returns approvals matching the filter, ordered by
// created_at DESC. Used by debug surfaces and the executor's resume
// pipeline.
type ListFilter struct {
	CoordSessionID string
	Statuses       []string
	Limit          int
}

func (c *Client) ListApprovals(ctx context.Context, f ListFilter) ([]ApprovalRecord, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	filter := map[string]any{"agent_id": map[string]any{"eq": c.agentID}}
	if f.CoordSessionID != "" {
		filter["coord_session_id"] = map[string]any{"eq": f.CoordSessionID}
	}
	if len(f.Statuses) > 0 {
		filter["status"] = map[string]any{"in": f.Statuses}
	}
	rows, err := queries.RunQuery[[]approvalRow](ctx, c.querier,
		`query ($filter: hub_db_approvals_filter, $limit: Int!) {
			hub { db { agent {
				approvals(
					filter: $filter
					order_by: [{field: "created_at", direction: DESC}]
					limit: $limit
				) {
					id agent_id mission_session_id coord_session_id
					tool_name args risk status response
					created_at responded_at
				}
			}}}
		}`,
		map[string]any{"filter": filter, "limit": limit},
		"hub.db.agent.approvals",
	)
	if err != nil {
		if errors.Is(err, types.ErrNoData) || errors.Is(err, types.ErrWrongDataPath) {
			return nil, nil
		}
		return nil, fmt.Errorf("approvals/store: list approvals: %w", err)
	}
	out := make([]ApprovalRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.toRecord())
	}
	return out, nil
}

// UpdateStatus transitions an approval row from `pending` to one of
// the terminal statuses. Returns ErrApprovalNotFound if the row does
// not exist; ErrAlreadyResolved if the row's status is non-pending.
// The check + write is one statement (UPDATE ... WHERE status='pending')
// so concurrent races resolve cleanly.
func (c *Client) UpdateStatus(ctx context.Context, id, newStatus string, response map[string]any, respondedAt time.Time) error {
	if id == "" || newStatus == "" {
		return fmt.Errorf("approvals/store: UpdateStatus requires id + newStatus")
	}
	upd := map[string]any{
		"status":       newStatus,
		"responded_at": respondedAt,
	}
	if response != nil {
		upd["response"] = response
	}
	type updateResult struct {
		Affected int `json:"affected_rows"`
	}
	res, err := queries.RunQuery[updateResult](ctx, c.querier,
		`mutation ($filter: hub_db_approvals_filter!, $data: hub_db_approvals_mut_data!) {
			hub { db { agent {
				update_approvals(filter: $filter, data: $data) { affected_rows }
			}}}
		}`,
		map[string]any{
			"filter": map[string]any{
				"id":     map[string]any{"eq": id},
				"status": map[string]any{"eq": "pending"},
			},
			"data": upd,
		},
		"hub.db.agent.update_approvals",
	)
	if err != nil {
		return fmt.Errorf("approvals/store: update status: %w", err)
	}
	if res.Affected == 0 {
		// Distinguish "no row at all" from "row exists but already terminal".
		existing, getErr := c.GetApproval(ctx, id)
		if getErr != nil {
			return getErr // ErrApprovalNotFound when missing
		}
		if existing.Status == "expired" {
			return ErrApprovalExpired
		}
		return ErrAlreadyResolved
	}
	return nil
}

// SweepExpired marks every pending row older than cutoff as expired
// in a single bulk update. Returns the affected row identifiers so
// the caller can emit one approval_responded event per row.
func (c *Client) SweepExpired(ctx context.Context, cutoff time.Time) ([]ApprovalRecord, error) {
	// Two-step: read pending+aged ids, then update them. We could do
	// this in one mutation with `RETURNING` on Postgres, but the
	// engine's DuckDB path does not surface RETURNING through the
	// GraphQL mutation shape uniformly. Two-step keeps both backends
	// happy and lets the caller emit events for each id.
	rows, err := queries.RunQuery[[]approvalRow](ctx, c.querier,
		`query ($agent: String!, $cutoff: Timestamp!) {
			hub { db { agent {
				approvals(
					filter: {
						agent_id: {eq: $agent}
						status: {eq: "pending"}
						created_at: {lt: $cutoff}
					}
					limit: 1000
				) {
					id agent_id mission_session_id coord_session_id
					tool_name args risk status response
					created_at responded_at
				}
			}}}
		}`,
		map[string]any{"agent": c.agentID, "cutoff": cutoff.UTC().Format(time.RFC3339)},
		"hub.db.agent.approvals",
	)
	if err != nil {
		if errors.Is(err, types.ErrNoData) || errors.Is(err, types.ErrWrongDataPath) {
			return nil, nil
		}
		return nil, fmt.Errorf("approvals/store: list pending: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}

	now := time.Now().UTC()
	out := make([]ApprovalRecord, 0, len(rows))
	for _, r := range rows {
		rec := r.toRecord()
		if err := c.UpdateStatus(ctx, r.ID, "expired", nil, now); err != nil {
			// If a concurrent Respond won the race, skip this id —
			// it's already terminal. ErrAlreadyResolved is expected.
			if errors.Is(err, ErrAlreadyResolved) || errors.Is(err, ErrApprovalExpired) || errors.Is(err, ErrApprovalNotFound) {
				continue
			}
			return out, err
		}
		rec.Status = "expired"
		rec.RespondedAt = &now
		out = append(out, rec)
	}
	return out, nil
}

