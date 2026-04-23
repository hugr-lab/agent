package memory

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestService_ToolNames — the service exposes the expected tools when a
// querier is provided; returns an empty tool list when querier is nil.
func TestService_ToolNames(t *testing.T) {
	// Nil querier → no tools (provider registers but exposes empty list).
	svc, err := NewService(nil, nil, nil, ServiceOptions{AgentID: "ag01"})
	require.NoError(t, err)
	assert.Empty(t, svc.Tools())

	// With a (stub) querier → five tools by name. We only exercise the
	// construction path here; no tool Run is invoked.
	svc, err = NewService(stubQuerier{}, nil, nil, ServiceOptions{AgentID: "ag01", AgentShort: "ag01"})
	require.NoError(t, err)
	tools := svc.Tools()
	require.Len(t, tools, 5)
	names := make([]string, 0, len(tools))
	for _, tl := range tools {
		names = append(names, tl.Name())
	}
	assert.ElementsMatch(t, []string{
		"memory_search", "memory_linked", "memory_stats",
		"memory_note", "memory_clear_note",
	}, names)
}

func TestService_Name(t *testing.T) {
	svc, err := NewService(nil, nil, nil, ServiceOptions{AgentID: "ag01"})
	require.NoError(t, err)
	assert.Equal(t, "_memory", svc.Name())
}
