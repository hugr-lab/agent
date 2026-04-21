package chatcontext

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewService_ToolNames — the service always exposes context_status
// and context_intro; hub may be nil.
func TestNewService_ToolNames(t *testing.T) {
	svc := NewService(nil, nil)
	tools := svc.Tools()
	require.Len(t, tools, 2)
	names := make([]string, 0, len(tools))
	for _, tl := range tools {
		names = append(names, tl.Name())
	}
	assert.ElementsMatch(t, []string{"context_status", "context_intro"}, names)
}

func TestService_Name(t *testing.T) {
	svc := NewService(nil, nil)
	assert.Equal(t, "_context", svc.Name())
}
