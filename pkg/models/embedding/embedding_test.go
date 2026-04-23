package embedding

import (
	"context"
	"errors"
	"testing"

	"github.com/hugr-lab/query-engine/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubQuerier returns a canned Response or an error from Query, and
// tracks call count + arguments for assertions. The response is
// rebuilt per-call because queries.RunQuery calls Close() which wipes
// Data — otherwise subsequent calls would see ErrNoData.
type stubQuerier struct {
	vector  []float64
	err     error
	calls   int
	lastVar map[string]any
}

func (s *stubQuerier) Query(_ context.Context, _ string, vars map[string]any) (*types.Response, error) {
	s.calls++
	s.lastVar = vars
	if s.err != nil {
		return nil, s.err
	}
	return wrap(s.vector), nil
}
func (s *stubQuerier) Subscribe(context.Context, string, map[string]any) (*types.Subscription, error) {
	return nil, nil
}
func (s *stubQuerier) RegisterDataSource(context.Context, types.DataSource) error       { return nil }
func (s *stubQuerier) LoadDataSource(context.Context, string) error                     { return nil }
func (s *stubQuerier) UnloadDataSource(context.Context, string, ...types.UnloadOpt) error { return nil }
func (s *stubQuerier) DataSourceStatus(context.Context, string) (string, error)         { return "", nil }
func (s *stubQuerier) DescribeDataSource(context.Context, string, bool) (string, error) { return "", nil }

// wrap builds the nested function.core.models.embedding response.
func wrap(vector []float64) *types.Response {
	return &types.Response{Data: map[string]any{
		"function": map[string]any{
			"core": map[string]any{
				"models": map[string]any{
					"embedding": map[string]any{
						"vector": vector,
					},
				},
			},
		},
	}}
}

func TestClient_Available(t *testing.T) {
	assert.False(t, New(nil, Options{}).Available())
	assert.False(t, New(nil, Options{Model: "m"}).Available(), "dim=0 disables")
	assert.False(t, New(nil, Options{Dimension: 768}).Available(), "empty model disables")
	assert.True(t, New(nil, Options{Model: "m", Dimension: 768}).Available())
}

func TestClient_Dimension(t *testing.T) {
	c := New(nil, Options{Model: "m", Dimension: 1024})
	assert.Equal(t, 1024, c.Dimension())
}

func TestEmbed_DisabledReturnsErrDisabled(t *testing.T) {
	c := New(nil, Options{})
	_, err := c.Embed(context.Background(), "hi")
	require.ErrorIs(t, err, ErrDisabled)

	_, err = c.EmbedBatch(context.Background(), []string{"a", "b"})
	require.ErrorIs(t, err, ErrDisabled)
}

func TestEmbed_VectorConvertsToFloat32(t *testing.T) {
	q := &stubQuerier{vector: []float64{0.1, 0.2, 0.3}}
	c := New(q, Options{Model: "m", Dimension: 3})

	v, err := c.Embed(context.Background(), "hello")
	require.NoError(t, err)
	require.Len(t, v, 3)
	assert.InDelta(t, 0.1, v[0], 1e-6)
	assert.InDelta(t, 0.2, v[1], 1e-6)
	assert.InDelta(t, 0.3, v[2], 1e-6)

	// Variables forwarded to the GraphQL call.
	assert.Equal(t, "m", q.lastVar["model"])
	assert.Equal(t, "hello", q.lastVar["input"])
}

func TestEmbed_QuerierError(t *testing.T) {
	q := &stubQuerier{err: errors.New("network down")}
	c := New(q, Options{Model: "m", Dimension: 3})

	_, err := c.Embed(context.Background(), "hi")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "network down")
}

func TestEmbedBatch_CallsEmbedPerText(t *testing.T) {
	q := &stubQuerier{vector: []float64{1, 2, 3}}
	c := New(q, Options{Model: "m", Dimension: 3})

	vecs, err := c.EmbedBatch(context.Background(), []string{"a", "b", "c"})
	require.NoError(t, err)
	assert.Len(t, vecs, 3)
	assert.Equal(t, 3, q.calls)
}
