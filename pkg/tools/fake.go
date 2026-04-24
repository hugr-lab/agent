package tools

import (
	"google.golang.org/adk/tool"
	"google.golang.org/genai"

	"google.golang.org/adk/model"
)

// FakeTool is a fixed-name tool.Tool with no declaration / run logic —
// enough for provider-manifest assertions in tests.
type FakeTool struct {
	N string
}

func (t FakeTool) Name() string        { return t.N }
func (t FakeTool) Description() string { return t.N }
func (t FakeTool) IsLongRunning() bool { return false }

// RunnableFakeTool is a tool.Tool that ADK's runner will actually
// invoke end-to-end: implements Declaration + ProcessRequest + Run
// the same way the production tools do, but delegates Run to a
// caller-provided closure so tests can inject success / error /
// slow paths without wiring a real provider.
type RunnableFakeTool struct {
	N       string
	D       string
	RunFunc func(ctx tool.Context, args any) (map[string]any, error)
}

func (t *RunnableFakeTool) Name() string {
	if t.N == "" {
		return "fake"
	}
	return t.N
}
func (t *RunnableFakeTool) Description() string {
	if t.D == "" {
		return t.Name()
	}
	return t.D
}
func (t *RunnableFakeTool) IsLongRunning() bool { return false }
func (t *RunnableFakeTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: t.Name(), Description: t.Description()}
}
func (t *RunnableFakeTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	Pack(req, t)
	return nil
}
func (t *RunnableFakeTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	if t.RunFunc != nil {
		return t.RunFunc(ctx, args)
	}
	return map[string]any{"ok": true}, nil
}

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
