package memory

import (
	"testing"

	"github.com/hugr-lab/hugen/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestService_ToolNames — the service exposes the expected tools when
// hub is provided; returns an empty tool list when hub is missing.
func TestService_ToolNames(t *testing.T) {
	// Nil hub → no tools (provider registers but exposes empty list).
	svc := NewService(nil, nil)
	assert.Empty(t, svc.Tools())

	// Real hub → five tools by name.
	svc = NewService(nil, stubHub{})
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
	assert.Equal(t, "_memory", NewService(nil, nil).Name())
}

// stubHub is a zero-value HubDB that returns empty results for every
// method. The real adapter is exercised by pkg/store/*_test.go; this
// package only verifies wiring.
type stubHub struct{ store.DB }
