package hubdb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/interfaces"
)

// GetAgentType fetches an agent type by ID. Returns (nil, nil) if not found.
func (h *hubDB) GetAgentType(ctx context.Context, typeID string) (*interfaces.AgentType, error) {
	type row struct {
		ID          string         `json:"id"`
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Config      map[string]any `json:"config"`
		CreatedAt   dbTime         `json:"created_at"`
		UpdatedAt   dbTime         `json:"updated_at"`
	}
	rows, err := runQuery[[]row](ctx, h.querier,
		`query ($id: String!) {
			hub { db {
				agent_types(filter: {id: {eq: $id}}, limit: 1) {
					id name description config created_at updated_at
				}
			}}
		}`,
		map[string]any{"id": typeID},
		"hub.db.agent_types",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	r := rows[0]
	return &interfaces.AgentType{
		ID:          r.ID,
		Name:        r.Name,
		Description: r.Description,
		Config:      r.Config,
		CreatedAt:   r.CreatedAt.Time,
		UpdatedAt:   r.UpdatedAt.Time,
	}, nil
}

// GetAgent fetches an agent instance by ID. Returns (nil, nil) if not found.
func (h *hubDB) GetAgent(ctx context.Context, id string) (*interfaces.Agent, error) {
	type row struct {
		ID             string         `json:"id"`
		AgentTypeID    string         `json:"agent_type_id"`
		ShortID        string         `json:"short_id"`
		Name           string         `json:"name"`
		Status         string         `json:"status"`
		ConfigOverride map[string]any `json:"config_override"`
		CreatedAt      dbTime         `json:"created_at"`
		LastActive     dbTime         `json:"last_active"`
	}
	rows, err := runQuery[[]row](ctx, h.querier,
		`query ($id: String!) {
			hub { db {
				agents(filter: {id: {eq: $id}}, limit: 1) {
					id agent_type_id short_id name status config_override created_at last_active
				}
			}}
		}`,
		map[string]any{"id": id},
		"hub.db.agents",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	r := rows[0]
	return &interfaces.Agent{
		ID:             r.ID,
		AgentTypeID:    r.AgentTypeID,
		ShortID:        r.ShortID,
		Name:           r.Name,
		Status:         r.Status,
		ConfigOverride: r.ConfigOverride,
		CreatedAt:      r.CreatedAt.Time,
		LastActive:     r.LastActive.Time,
	}, nil
}

// RegisterAgent upserts the agent row. If one already exists with the same
// ID, it refreshes mutable fields (name, status, config_override, last_active)
// — agent_type_id and short_id stay pinned to what's already in the DB.
// Idempotent across restarts.
func (h *hubDB) RegisterAgent(ctx context.Context, a interfaces.Agent) error {
	if a.ID == "" {
		return fmt.Errorf("hubdb: RegisterAgent requires ID")
	}
	existing, err := h.GetAgent(ctx, a.ID)
	if err != nil {
		return err
	}

	override := a.ConfigOverride
	if override == nil {
		override = map[string]any{}
	}
	status := a.Status
	if status == "" {
		status = "active"
	}

	if existing != nil {
		now := time.Now().UTC().Format(time.RFC3339)
		return runMutation(ctx, h.querier,
			`mutation ($id: String!, $data: hub_db_agents_mut_data!) {
				hub { db {
					update_agents(filter: {id: {eq: $id}}, data: $data) { affected_rows }
				}}
			}`,
			map[string]any{
				"id": a.ID,
				"data": map[string]any{
					"name":            a.Name,
					"status":          status,
					"config_override": override,
					"last_active":     now,
				},
			},
		)
	}

	return runMutation(ctx, h.querier,
		`mutation ($data: hub_db_agents_mut_input_data!) {
			hub { db {
				insert_agents(data: $data) { id }
			}}
		}`,
		map[string]any{
			"data": map[string]any{
				"id":              a.ID,
				"agent_type_id":   a.AgentTypeID,
				"short_id":        a.ShortID,
				"name":            a.Name,
				"status":          status,
				"config_override": override,
			},
		},
	)
}

// UpdateAgentActivity bumps last_active to now.
func (h *hubDB) UpdateAgentActivity(ctx context.Context, id string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	return runMutation(ctx, h.querier,
		`mutation ($id: String!, $data: hub_db_agents_mut_data!) {
			hub { db {
				update_agents(filter: {id: {eq: $id}}, data: $data) { affected_rows }
			}}
		}`,
		map[string]any{
			"id":   id,
			"data": map[string]any{"last_active": now},
		},
	)
}
