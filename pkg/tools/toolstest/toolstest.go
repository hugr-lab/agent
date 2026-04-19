// Package toolstest offers tiny in-memory fixtures for unit tests that
// need a tools.Provider or a tool.Tool without pulling in MCP wiring.
package toolstest

import "google.golang.org/adk/tool"

// Tool is a fixed-name tool.Tool with no declaration / run logic —
// enough for provider-manifest assertions.
type Tool struct {
	N string
}

func (t Tool) Name() string        { return t.N }
func (t Tool) Description() string { return t.N }
func (t Tool) IsLongRunning() bool { return false }

// Provider is a fixed-list tools.Provider. Name is whatever you pass
// in; Tools returns the same slice on every call.
type Provider struct {
	N string
	T []tool.Tool
}

func (p Provider) Name() string       { return p.N }
func (p Provider) Tools() []tool.Tool { return p.T }

// Tools wraps raw names into Tool structs — convenience shortcut for
// `[]tool.Tool{Tool{N:"a"}, Tool{N:"b"}}`.
func Tools(names ...string) []tool.Tool {
	out := make([]tool.Tool, 0, len(names))
	for _, n := range names {
		out = append(out, Tool{N: n})
	}
	return out
}
