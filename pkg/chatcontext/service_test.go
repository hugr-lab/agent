package chatcontext

import (
	"context"
	"testing"

	"github.com/hugr-lab/query-engine/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubQuerier is a no-op types.Querier for construction-only tests.
type stubQuerier struct{}

func (stubQuerier) Query(ctx context.Context, q string, vars map[string]any) (*types.Response, error) {
	return nil, nil
}
func (stubQuerier) Subscribe(ctx context.Context, q string, vars map[string]any) (*types.Subscription, error) {
	return nil, nil
}
func (stubQuerier) RegisterDataSource(ctx context.Context, ds types.DataSource) error { return nil }
func (stubQuerier) LoadDataSource(ctx context.Context, name string) error             { return nil }
func (stubQuerier) UnloadDataSource(ctx context.Context, name string, opts ...types.UnloadOpt) error {
	return nil
}
func (stubQuerier) DataSourceStatus(ctx context.Context, name string) (string, error) {
	return "", nil
}
func (stubQuerier) DescribeDataSource(ctx context.Context, name string, self bool) (string, error) {
	return "", nil
}

// TestNewService_ToolNames — the service always exposes context_status
// and context_intro; querier may be nil.
func TestNewService_ToolNames(t *testing.T) {
	svc, err := NewService(nil, nil, ServiceOptions{AgentID: "ag01"})
	require.NoError(t, err)
	tools := svc.Tools()
	require.Len(t, tools, 2)
	names := make([]string, 0, len(tools))
	for _, tl := range tools {
		names = append(names, tl.Name())
	}
	assert.ElementsMatch(t, []string{"context_status", "context_intro"}, names)

	// With a querier stub the service builds its memory+session stores
	// and still exposes the same two tools.
	svc, err = NewService(stubQuerier{}, nil, ServiceOptions{AgentID: "ag01", AgentShort: "ag01"})
	require.NoError(t, err)
	require.Len(t, svc.Tools(), 2)
}

func TestService_Name(t *testing.T) {
	svc, err := NewService(nil, nil, ServiceOptions{AgentID: "ag01"})
	require.NoError(t, err)
	assert.Equal(t, "_context", svc.Name())
}
