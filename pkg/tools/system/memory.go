package system

import (
	"github.com/hugr-lab/hugen/interfaces"
	"google.golang.org/adk/tool"
)

// NewMemorySuite returns the memory-management tools (memory_note,
// memory_search, memory_reinforce, memory_hint, …). The full suite
// lands in spec 003b; in 004 the suite is empty so the `_memory`
// system provider can be declared in config.yaml without crashing.
func NewMemorySuite(sm interfaces.SessionManager, hub interfaces.HubDB) []tool.Tool {
	_ = sm
	_ = hub
	return nil
}
