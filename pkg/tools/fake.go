package tools

import "google.golang.org/adk/tool"

// FakeTool is a fixed-name tool.Tool with no declaration / run logic —
// enough for provider-manifest assertions in tests.
type FakeTool struct {
	N string
}

func (t FakeTool) Name() string        { return t.N }
func (t FakeTool) Description() string { return t.N }
func (t FakeTool) IsLongRunning() bool { return false }

// FakeProvider is a fixed-list Provider. Name is whatever you pass in;
// Tools returns the same slice on every call. Used only in tests.
type FakeProvider struct {
	N string
	T []tool.Tool
}

func (p FakeProvider) Name() string       { return p.N }
func (p FakeProvider) Tools() []tool.Tool { return p.T }

// FakeTools wraps raw names into FakeTool structs — convenience shortcut
// for `[]tool.Tool{FakeTool{N:"a"}, FakeTool{N:"b"}}`.
func FakeTools(names ...string) []tool.Tool {
	out := make([]tool.Tool, 0, len(names))
	for _, n := range names {
		out = append(out, FakeTool{N: n})
	}
	return out
}
