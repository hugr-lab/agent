package hubdb

import (
	"context"
	"fmt"

	"github.com/hugr-lab/hugen/interfaces"
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
func (h *hubDB) UpsertAgentType(ctx context.Context, at interfaces.AgentType) error {
	return notImplemented("UpsertAgentType")
}

// ListAgents is stubbed — an agent only reads and updates its own row.
// Cross-agent listing belongs on the hub side.
func (h *hubDB) ListAgents(ctx context.Context, typeID string) ([]interfaces.Agent, error) {
	return nil, notImplemented("ListAgents")
}
