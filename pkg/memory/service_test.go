package memory

import (
	"testing"

	memdb "github.com/hugr-lab/hugen/pkg/store/memory"
	sessdb "github.com/hugr-lab/hugen/pkg/store/sessions"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestService_ToolNames — the service exposes the expected tools when
// memory+sessions are provided; returns an empty tool list otherwise.
func TestService_ToolNames(t *testing.T) {
	// Nil clients → no tools (provider registers but exposes empty list).
	svc := NewService(nil, nil, nil, nil)
	assert.Empty(t, svc.Tools())

	// Real clients → five tools by name. We use zero-value Client
	// structs; the tools only need a pointer for wiring, not a working
	// querier, since this test never invokes Run.
	svc = NewService(nil, &memdb.Client{}, &sessdb.Client{}, nil)
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
	assert.Equal(t, "_memory", NewService(nil, nil, nil, nil).Name())
}
