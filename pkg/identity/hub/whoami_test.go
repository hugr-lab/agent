package hub

import (
	"context"
	"errors"
	"testing"

	"github.com/hugr-lab/query-engine/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubQuerier returns a canned Response (or error) from Query. The
// other Querier methods aren't exercised by WhoAmI.
type stubQuerier struct {
	resp *types.Response
	err  error
}

func (s *stubQuerier) Query(context.Context, string, map[string]any) (*types.Response, error) {
	return s.resp, s.err
}
func (s *stubQuerier) Subscribe(context.Context, string, map[string]any) (*types.Subscription, error) {
	return nil, nil
}
func (s *stubQuerier) RegisterDataSource(context.Context, types.DataSource) error { return nil }
func (s *stubQuerier) LoadDataSource(context.Context, string) error                { return nil }
func (s *stubQuerier) UnloadDataSource(context.Context, string, ...types.UnloadOpt) error {
	return nil
}
func (s *stubQuerier) DataSourceStatus(context.Context, string) (string, error)         { return "", nil }
func (s *stubQuerier) DescribeDataSource(context.Context, string, bool) (string, error) { return "", nil }

// wrap builds the nested function.core.auth.me response shape the
// ScanData path expects. The "me" leaf is an object (not a list).
func wrap(me map[string]any) *types.Response {
	return &types.Response{Data: map[string]any{
		"function": map[string]any{
			"core": map[string]any{
				"auth": map[string]any{
					"me": me,
				},
			},
		},
	}}
}

func TestWhoAmI_Happy(t *testing.T) {
	q := &stubQuerier{resp: wrap(map[string]any{
		"user_id":   "u-42",
		"user_name": "Alice",
		"role":      "admin",
	})}

	src := New(q)
	who, err := src.WhoAmI(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "u-42", who.UserID)
	assert.Equal(t, "Alice", who.UserName)
	assert.Equal(t, "admin", who.Role)
}

func TestWhoAmI_EmptyUserID(t *testing.T) {
	q := &stubQuerier{resp: wrap(map[string]any{
		"user_id":   "",
		"user_name": "Anon",
	})}
	_, err := New(q).WhoAmI(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty user_id")
}

func TestWhoAmI_QuerierError(t *testing.T) {
	q := &stubQuerier{err: errors.New("conn refused")}
	_, err := New(q).WhoAmI(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "conn refused")
}

func TestWhoAmI_MissingPath(t *testing.T) {
	// Response without the expected function.core.auth.me leaf →
	// ScanData surfaces ErrWrongDataPath → translated to empty
	// payload error.
	q := &stubQuerier{resp: &types.Response{Data: map[string]any{
		"function": map[string]any{"core": map[string]any{}},
	}}}
	_, err := New(q).WhoAmI(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty payload")
}
