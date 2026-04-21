package registry

import (
	"context"
	"fmt"
)

// notImplemented covers the remaining HubDB surface that belongs on
// the hub side (cross-agent operations) rather than in a per-agent
// standalone runtime. UpsertAgentType is seeded by migrations;
// ListAgents requires multi-agent visibility which a single-agent
// process does not have.
func notImplemented(op string) error {
	return fmt.Errorf("hubdb: %s not implemented", op)
}

// UpsertAgentType is stubbed — agent_types rows are seeded during the
// initial migration (adapters/hubdb/migrate). Per-agent customisation
// lives in agents.config_override, not a runtime upsert.
func (c *Client) UpsertAgentType(ctx context.Context, at AgentType) error {
	return notImplemented("UpsertAgentType")
}

// ListAgents is stubbed — an agent only reads and updates its own row.
// Cross-agent listing belongs on the hub side.
func (c *Client) ListAgents(ctx context.Context, typeID string) ([]Agent, error) {
	return nil, notImplemented("ListAgents")
}
