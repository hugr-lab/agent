package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/pkg/store/queries"
)

// PolicyRecord mirrors a tool_policies row.
type PolicyRecord struct {
	AgentID   string
	ToolName  string
	Scope     string
	Policy    string
	Note      string
	CreatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type policyRow struct {
	AgentID   string    `json:"agent_id"`
	ToolName  string    `json:"tool_name"`
	Scope     string    `json:"scope"`
	Policy    string    `json:"policy"`
	Note      string    `json:"note"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (r policyRow) toRecord() PolicyRecord {
	return PolicyRecord(r)
}

// UpsertPolicy inserts or updates a policy row by composite PK.
// On insert, created_at + updated_at are populated server-side; on
// update, only updated_at advances.
func (c *Client) UpsertPolicy(ctx context.Context, p PolicyRecord) error {
	if p.ToolName == "" || p.Scope == "" || p.Policy == "" {
		return fmt.Errorf("approvals/store: UpsertPolicy requires ToolName + Scope + Policy")
	}
	if p.AgentID == "" {
		p.AgentID = c.agentID
	}
	if p.CreatedBy == "" {
		p.CreatedBy = "system"
	}
	data := map[string]any{
		"agent_id":   p.AgentID,
		"tool_name":  p.ToolName,
		"scope":      p.Scope,
		"policy":     p.Policy,
		"note":       p.Note,
		"created_by": p.CreatedBy,
		"updated_at": time.Now().UTC().Format(time.RFC3339),
	}

	// Try update first (matches the existing PK); fall back to insert
	// if update reports zero rows. Two-step pattern keeps both
	// DuckDB and Postgres happy without an INSERT...ON CONFLICT shape.
	type updateResult struct {
		Affected int `json:"affected_rows"`
	}
	res, err := queries.RunQuery[updateResult](ctx, c.querier,
		`mutation ($filter: hub_db_tool_policies_filter!, $data: hub_db_tool_policies_mut_data!) {
			hub { db { agent {
				update_tool_policies(filter: $filter, data: $data) { affected_rows }
			}}}
		}`,
		map[string]any{
			"filter": map[string]any{
				"agent_id":  map[string]any{"eq": p.AgentID},
				"tool_name": map[string]any{"eq": p.ToolName},
				"scope":     map[string]any{"eq": p.Scope},
			},
			"data": data,
		},
		"hub.db.agent.update_tool_policies",
	)
	if err != nil {
		return fmt.Errorf("approvals/store: update policy: %w", err)
	}
	if res.Affected > 0 {
		return nil
	}
	// No existing row → insert.
	return queries.RunMutation(ctx, c.querier,
		`mutation ($data: hub_db_tool_policies_mut_input_data!) {
			hub { db { agent {
				insert_tool_policies(data: $data) { agent_id tool_name scope }
			}}}
		}`,
		map[string]any{"data": data},
	)
}

// DeletePolicy removes one row by composite PK. Returns existed=true
// if a row was deleted.
func (c *Client) DeletePolicy(ctx context.Context, agentID, toolName, scope string) (bool, error) {
	if toolName == "" || scope == "" {
		return false, fmt.Errorf("approvals/store: DeletePolicy requires toolName + scope")
	}
	if agentID == "" {
		agentID = c.agentID
	}
	type deleteResult struct {
		Affected int `json:"affected_rows"`
	}
	res, err := queries.RunQuery[deleteResult](ctx, c.querier,
		`mutation ($filter: hub_db_tool_policies_filter!) {
			hub { db { agent {
				delete_tool_policies(filter: $filter) { affected_rows }
			}}}
		}`,
		map[string]any{
			"filter": map[string]any{
				"agent_id":  map[string]any{"eq": agentID},
				"tool_name": map[string]any{"eq": toolName},
				"scope":     map[string]any{"eq": scope},
			},
		},
		"hub.db.agent.delete_tool_policies",
	)
	if err != nil {
		return false, fmt.Errorf("approvals/store: delete policy: %w", err)
	}
	return res.Affected > 0, nil
}

// LoadAllPolicies returns every policy row for the agent. Called
// once at PolicyStore construction (and on Refresh) to seed the hot
// snapshot.
func (c *Client) LoadAllPolicies(ctx context.Context) ([]PolicyRecord, error) {
	rows, err := queries.RunQuery[[]policyRow](ctx, c.querier,
		`query ($agent: String!) {
			hub { db { agent {
				tool_policies(filter: {agent_id: {eq: $agent}}, limit: 10000) {
					agent_id tool_name scope policy note created_by created_at updated_at
				}
			}}}
		}`,
		map[string]any{"agent": c.agentID},
		"hub.db.agent.tool_policies",
	)
	if err != nil {
		if errors.Is(err, types.ErrNoData) || errors.Is(err, types.ErrWrongDataPath) {
			return nil, nil
		}
		return nil, fmt.Errorf("approvals/store: load policies: %w", err)
	}
	out := make([]PolicyRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.toRecord())
	}
	return out, nil
}

// GetPolicy returns one row by composite PK. Returns ErrNotFound
// (sentinel below) when the row does not exist. Used by tests +
// safe_policy_change diagnostics.
func (c *Client) GetPolicy(ctx context.Context, agentID, toolName, scope string) (PolicyRecord, error) {
	if agentID == "" {
		agentID = c.agentID
	}
	rows, err := queries.RunQuery[[]policyRow](ctx, c.querier,
		`query ($agent: String!, $tool: String!, $scope: String!) {
			hub { db { agent {
				tool_policies(filter: {
					agent_id: {eq: $agent}
					tool_name: {eq: $tool}
					scope: {eq: $scope}
				}, limit: 1) {
					agent_id tool_name scope policy note created_by created_at updated_at
				}
			}}}
		}`,
		map[string]any{"agent": agentID, "tool": toolName, "scope": scope},
		"hub.db.agent.tool_policies",
	)
	if err != nil {
		if errors.Is(err, types.ErrNoData) || errors.Is(err, types.ErrWrongDataPath) {
			return PolicyRecord{}, ErrPolicyNotFound
		}
		return PolicyRecord{}, fmt.Errorf("approvals/store: get policy: %w", err)
	}
	if len(rows) == 0 {
		return PolicyRecord{}, ErrPolicyNotFound
	}
	return rows[0].toRecord(), nil
}

// ErrPolicyNotFound is the sentinel for missing tool_policies rows.
var ErrPolicyNotFound = errors.New("approvals/store: policy not found")

// ErrApprovalNotFound is the sentinel for missing approvals rows.
var ErrApprovalNotFound = errors.New("approvals/store: approval not found")

// ErrAlreadyResolved is returned by UpdateStatus when the row's
// status is already non-pending.
var ErrAlreadyResolved = errors.New("approvals/store: already resolved")

// ErrApprovalExpired is the more-specific form of ErrAlreadyResolved
// when the prior terminal state was 'expired'.
var ErrApprovalExpired = errors.New("approvals/store: already expired")
