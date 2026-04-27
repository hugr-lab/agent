package local

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/identity"
)

// Agent reads the YAML at configPath and returns it as the agent's
// merged config map. The agent.* sub-block populates the identity
// fields (id, short_id, name, type / agent_type_id) — falling back
// to the literal "local" string when the YAML omits them.
//
// In hybrid mode (NewWithHub) the hub provides the authoritative
// agents row first; YAML keys then overlay it.
func (s *Source) Agent(ctx context.Context) (identity.Agent, error) {
	var agent identity.Agent
	if s.hub != nil {
		hubAgent, err := s.hub.Agent(ctx)
		if err != nil {
			return identity.Agent{}, err
		}
		agent = hubAgent
	} else {
		agent = identity.Agent{
			ID:          "local",
			AgentTypeID: "local",
			Type:        "local",
			ShortID:     "local",
			Name:        "local",
			Status:      "active",
		}
	}

	if s.configPath == "" {
		return agent, nil
	}
	cfg, err := loadLocalConfig(s.configPath)
	if err != nil {
		return identity.Agent{}, err
	}
	if cfg == nil {
		return agent, nil
	}

	// Promote agent.* identity fields from the YAML when present —
	// hub-supplied values stay only for keys the YAML doesn't set.
	if a, ok := cfg["agent"].(map[string]any); ok {
		agent.ID = stringFromMap(a, "id", agent.ID)
		agent.ShortID = stringFromMap(a, "short_id", agent.ShortID)
		agent.Name = stringFromMap(a, "name", agent.Name)
		if t := stringFromMap(a, "type", ""); t != "" {
			agent.Type = t
			agent.AgentTypeID = t
		}
	}
	agent.Config = cfg
	return agent, nil
}
