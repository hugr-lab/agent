package system

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewContextSuite_ToolNames — the suite always returns the three
// context-management tools; compactor may be nil.
func TestNewContextSuite_ToolNames(t *testing.T) {
	suite := NewContextSuite(nil, nil, nil)
	require.Len(t, suite, 3)
	names := make([]string, 0, len(suite))
	for _, tl := range suite {
		names = append(names, tl.Name())
	}
	assert.ElementsMatch(t, []string{
		"context_status", "context_intro", "context_compress",
	}, names)
}
