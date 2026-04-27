package hub

import (
	"context"
	"errors"
	"fmt"

	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/store/queries"
)

// Agent resolves the running agent instance and its merged config:
//
//	result.Config = agent_type.config (defaults)
//	              ⊕ agents.config_override (shallow top-level overlay)
//
// agentID is resolved lazily via WhoAmI on the first call when the
// Source was built without one. Subsequent calls reuse it.
//
// Fails loudly if the agents row is missing — in remote mode the
// hub is the authoritative registry, so its absence signals
// misconfiguration rather than something we should paper over.
func (s *Source) Agent(ctx context.Context) (identity.Agent, error) {
	agentID, err := s.resolveAgentID(ctx)
	if err != nil {
		return identity.Agent{}, err
	}

	row, err := fetchAgent(ctx, s.qe, agentID)
	if err != nil {
		return identity.Agent{}, err
	}
	if row == nil {
		return identity.Agent{}, fmt.Errorf("identity hub: agent %q not registered in hub — run hub-side registration first", agentID)
	}

	typeCfg, err := fetchAgentTypeConfig(ctx, s.qe, row.AgentTypeID)
	if err != nil {
		return identity.Agent{}, err
	}

	row.Config = mergeTopLevel(typeCfg, row.Config)
	row.Type = row.AgentTypeID
	return *row, nil
}

func (s *Source) resolveAgentID(ctx context.Context) (string, error) {
	s.mu.Lock()
	id := s.agentID
	s.mu.Unlock()
	if id != "" {
		return id, nil
	}
	who, err := s.WhoAmI(ctx)
	if err != nil {
		return "", fmt.Errorf("identity hub: resolve agent id via whoami: %w", err)
	}
	s.mu.Lock()
	if s.agentID == "" {
		s.agentID = who.UserID
	}
	id = s.agentID
	s.mu.Unlock()
	return id, nil
}

// mergeTopLevel returns a shallow merge of base and overlay by
// top-level keys. overlay wins where both define a key. Neither
// input is mutated. A nil map is treated as empty.
func mergeTopLevel(base, overlay map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(overlay))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overlay {
		out[k] = v
	}
	return out
}

func fetchAgent(ctx context.Context, q types.Querier, id string) (*identity.Agent, error) {
	type row struct {
		ID             string         `json:"id"`
		AgentTypeID    string         `json:"agent_type_id"`
		ShortID        string         `json:"short_id"`
		Name           string         `json:"name"`
		Status         string         `json:"status"`
		ConfigOverride map[string]any `json:"config_override"`
	}
	rows, err := queries.RunQuery[[]row](ctx, q,
		`query ($id: String!) {
			hub { db {
				agents(filter: {id: {eq: $id}}, limit: 1) {
					id agent_type_id short_id name status config_override
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
		return nil, fmt.Errorf("identity hub: fetch agent %q: %w", id, err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	r := rows[0]
	return &identity.Agent{
		ID:          r.ID,
		AgentTypeID: r.AgentTypeID,
		ShortID:     r.ShortID,
		Name:        r.Name,
		Status:      r.Status,
		Config:      r.ConfigOverride,
	}, nil
}

func fetchAgentTypeConfig(ctx context.Context, q types.Querier, typeID string) (map[string]any, error) {
	type row struct {
		Config map[string]any `json:"config"`
	}
	rows, err := queries.RunQuery[[]row](ctx, q,
		`query ($id: String!) {
			hub { db {
				agent_types(filter: {id: {eq: $id}}, limit: 1) {
					config
				}
			}}
		}`,
		map[string]any{"id": typeID},
		"hub.db.agent_types",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, fmt.Errorf("identity hub: agent_type %q not found in hub", typeID)
		}
		return nil, fmt.Errorf("identity hub: fetch agent_type %q: %w", typeID, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("identity hub: agent_type %q not found in hub", typeID)
	}
	if rows[0].Config == nil {
		return map[string]any{}, nil
	}
	return rows[0].Config, nil
}
