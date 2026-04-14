package systemtools

import (
	"google.golang.org/adk/agent"
	"google.golang.org/adk/tool"
)

// SystemToolset bundles all system tools into a single tool.Toolset.
type SystemToolset struct {
	tools []tool.Tool
}

var _ tool.Toolset = (*SystemToolset)(nil)

// NewSystemToolset creates a toolset with skill-list, skill-load, skill-ref,
// and context-status tools.
func NewSystemToolset(deps *Deps) *SystemToolset {
	return &SystemToolset{
		tools: []tool.Tool{
			&skillListTool{deps: deps},
			&skillLoadTool{deps: deps},
			&skillRefTool{deps: deps},
			&contextStatusTool{prompt: deps.Prompt, tokens: deps.Tokens},
		},
	}
}

func (s *SystemToolset) Name() string { return "system" }

func (s *SystemToolset) Tools(_ agent.ReadonlyContext) ([]tool.Tool, error) {
	return s.tools, nil
}
