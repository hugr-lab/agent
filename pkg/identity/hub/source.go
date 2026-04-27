// Package hub is the remote-mode identity.Source: it resolves the
// running agent + its principal by querying the hugr platform.
//
// The hub schema (template-driven migration in pkg/store/local/migrate)
// keeps agents and agent_types as separate tables — there is no
// server-side `agent_info` mutation that merges them. The merge
// happens client-side in this package: Agent() runs two queries
// (agent + agent_type) and overlays config_override on top of
// agent_type.config.
package hub

import (
	"sync"

	"github.com/hugr-lab/query-engine/types"
)

// Source talks to a hugr platform via types.Querier. agentID is
// resolved lazily via WhoAmI on the first Agent() call — keeping
// the Source interface itself parameter-free.
type Source struct {
	qe types.Querier

	mu      sync.Mutex
	agentID string
}

// New builds a hub Source that uses qe for every GraphQL roundtrip.
// agentID is left empty — Agent() resolves it via WhoAmI on first
// call. Callers that already know it can use NewWithAgent.
func New(qe types.Querier) *Source {
	return &Source{qe: qe}
}

// NewWithAgent pins the agent ID upfront, skipping the WhoAmI
// resolution Agent() would otherwise do. Used by tests + scenarios
// where the agent row is seeded directly and there is no auth
// principal to derive an ID from.
func NewWithAgent(qe types.Querier, agentID string) *Source {
	return &Source{qe: qe, agentID: agentID}
}

// AgentID returns the resolved agent ID (empty before the first
// Agent() call when constructed via New).
func (s *Source) AgentID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agentID
}
