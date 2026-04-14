package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/tool"
)

type stubToolset struct {
	name  string
	tools []tool.Tool
}

func (s *stubToolset) Name() string                                    { return s.name }
func (s *stubToolset) Tools(_ agent.ReadonlyContext) ([]tool.Tool, error) { return s.tools, nil }

type stubTool struct {
	name string
}

func (t *stubTool) Name() string        { return t.name }
func (t *stubTool) Description() string { return "" }
func (t *stubTool) IsLongRunning() bool { return false }

func TestDynamicToolset_AddRemove(t *testing.T) {
	dt := NewDynamicToolset()

	assert.Equal(t, "dynamic", dt.Name())
	assert.False(t, dt.HasToolset("ts1"))

	ts1 := &stubToolset{name: "ts1", tools: []tool.Tool{&stubTool{name: "t1"}, &stubTool{name: "t2"}}}
	dt.AddToolset("ts1", ts1)
	assert.True(t, dt.HasToolset("ts1"))

	tools, err := dt.Tools(nil)
	require.NoError(t, err)
	assert.Len(t, tools, 2)

	ts2 := &stubToolset{name: "ts2", tools: []tool.Tool{&stubTool{name: "t3"}}}
	dt.AddToolset("ts2", ts2)

	tools, err = dt.Tools(nil)
	require.NoError(t, err)
	assert.Len(t, tools, 3)

	dt.RemoveToolset("ts1")
	assert.False(t, dt.HasToolset("ts1"))

	tools, err = dt.Tools(nil)
	require.NoError(t, err)
	assert.Len(t, tools, 1)
	assert.Equal(t, "t3", tools[0].Name())
}

func TestDynamicToolset_Replace(t *testing.T) {
	dt := NewDynamicToolset()

	ts1 := &stubToolset{name: "ts1", tools: []tool.Tool{&stubTool{name: "old"}}}
	dt.AddToolset("ts1", ts1)

	ts1v2 := &stubToolset{name: "ts1", tools: []tool.Tool{&stubTool{name: "new1"}, &stubTool{name: "new2"}}}
	dt.AddToolset("ts1", ts1v2)

	tools, err := dt.Tools(nil)
	require.NoError(t, err)
	assert.Len(t, tools, 2)
}

func TestDynamicToolset_EmptyTools(t *testing.T) {
	dt := NewDynamicToolset()

	tools, err := dt.Tools(nil)
	require.NoError(t, err)
	assert.Empty(t, tools)
}
