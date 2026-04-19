package tools

import (
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/adk/tool"
)

type stubTool struct{ name string }

func (s stubTool) Name() string        { return s.name }
func (s stubTool) Description() string { return s.name }
func (s stubTool) IsLongRunning() bool { return false }

type stubProvider struct {
	name  string
	tools []tool.Tool
}

func (p stubProvider) Name() string       { return p.name }
func (p stubProvider) Tools() []tool.Tool { return p.tools }

type cachingStub struct {
	stubProvider
	invalidations int32
}

func (c *cachingStub) Invalidate() { atomic.AddInt32(&c.invalidations, 1) }

func TestFilteredProvider_PassThrough(t *testing.T) {
	raw := stubProvider{name: "raw", tools: []tool.Tool{
		stubTool{"a"}, stubTool{"b"}, stubTool{"discovery-x"},
	}}
	f := NewFiltered("view", raw, nil)

	names := toolNames(f.Tools())
	assert.ElementsMatch(t, []string{"a", "b", "discovery-x"}, names)
}

func TestFilteredProvider_ExactAndPrefix(t *testing.T) {
	raw := stubProvider{name: "raw", tools: []tool.Tool{
		stubTool{"discovery-sources"}, stubTool{"discovery-tables"},
		stubTool{"schema-types"}, stubTool{"data-query"},
	}}
	f := NewFiltered("view", raw, []string{"discovery-*", "schema-types"})

	names := toolNames(f.Tools())
	assert.ElementsMatch(t,
		[]string{"discovery-sources", "discovery-tables", "schema-types"},
		names)
}

func TestFilteredProvider_InvalidateForwards(t *testing.T) {
	raw := &cachingStub{stubProvider: stubProvider{name: "raw"}}
	f := NewFiltered("view", raw, nil)
	f.Invalidate()
	f.Invalidate()
	assert.Equal(t, int32(2), atomic.LoadInt32(&raw.invalidations))
}

func TestFilteredProvider_InvalidateOnPlainProvider_NoOp(t *testing.T) {
	raw := stubProvider{name: "raw"}
	f := NewFiltered("view", raw, nil)
	// Should not panic, should not do anything observable.
	f.Invalidate()
}

func toolNames(ts []tool.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name())
	}
	return out
}
