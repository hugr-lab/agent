package providers

import (
	"fmt"

	"github.com/hugr-lab/hugen/internal/config"
	"github.com/hugr-lab/hugen/pkg/tools"
	"github.com/hugr-lab/hugen/pkg/tools/system"
	"google.golang.org/adk/tool"
)

func init() {
	Register("system", buildSystem)
}

// buildSystem dispatches on cfg.Suite to produce a static tool list
// backed by an internal suite (skills lifecycle, memory, future
// delegation). The returned Provider is a staticProvider — its Tools()
// list is fixed at construction; suites embed SessionManager or other
// deps they need internally.
func buildSystem(cfg config.ProviderConfig, deps Deps) (tools.Provider, error) {
	var list []tool.Tool
	switch cfg.Suite {
	case "skills":
		if deps.Sessions == nil {
			return nil, fmt.Errorf("provider %q: system/skills needs SessionManager", cfg.Name)
		}
		list = system.NewSkillsSuite(deps.Sessions)
	case "memory":
		// 003b fills this; in 004 the suite is an empty list so
		// sessions referencing _memory load cleanly with no tools.
		list = system.NewMemorySuite(deps.Sessions, deps.Hub)
	default:
		return nil, fmt.Errorf("provider %q: unknown system suite %q", cfg.Name, cfg.Suite)
	}
	return &staticProvider{name: cfg.Name, tools: list}, nil
}

// staticProvider is a simple tools.Provider with a fixed list.
type staticProvider struct {
	name  string
	tools []tool.Tool
}

func (p *staticProvider) Name() string       { return p.name }
func (p *staticProvider) Tools() []tool.Tool { return p.tools }
