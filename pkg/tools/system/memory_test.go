package system

import (
	"testing"

	"github.com/hugr-lab/hugen/interfaces"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewMemorySuite_WiredWithHub — suite exposes the expected tools
// when hub is provided; returns nil when hub is missing.
func TestNewMemorySuite_WiredWithHub(t *testing.T) {
	// Nil hub → no tools (provider loads empty).
	assert.Nil(t, NewMemorySuite(nil, nil))

	// Real hub → five tools by name: read path + scratchpad.
	suite := NewMemorySuite(nil, stubHub{})
	require.Len(t, suite, 5)
	names := make([]string, 0, len(suite))
	for _, tl := range suite {
		names = append(names, tl.Name())
	}
	assert.ElementsMatch(t, []string{
		"memory_search", "memory_linked", "memory_stats",
		"memory_note", "memory_clear_note",
	}, names)
}

// stubHub is a zero-value HubDB that returns empty results for every
// method. The real adapter is exercised by adapters/hubdb/*_test.go;
// this package only verifies wiring.
type stubHub struct{ interfaces.HubDB }
