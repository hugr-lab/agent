package memory

import (
	"context"

	"github.com/hugr-lab/query-engine/types"
)

// stubQuerier is a no-op types.Querier used by unit tests that only
// exercise construction paths (no query is actually issued).
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
