package tools

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubStore is a trivial TokenStore — returns the same token forever.
type stubStore struct{ tok string }

func (s stubStore) Token(context.Context) (string, error) { return s.tok, nil }

// captureServer records the last request headers in got. Used to
// verify that the RoundTripper picked by mcpTransport sets the
// expected auth header.
func captureServer(t *testing.T, got *http.Header) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func roundTripAndAssertHeader(t *testing.T, rt http.RoundTripper, srvURL, header, expected string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srvURL+"/x", nil)
	require.NoError(t, err)
	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func TestMCPTransport_HugrMode(t *testing.T) {
	var got http.Header
	srv := captureServer(t, &got)

	rt, err := mcpTransport(MCPSpec{
		Name:          "mcp",
		AuthType:      "hugr",
		Auth:          "hugr",
		AuthStores:    map[string]auth.TokenStore{"hugr": stubStore{tok: "bearer-123"}},
		BaseTransport: http.DefaultTransport,
	})
	require.NoError(t, err)
	roundTripAndAssertHeader(t, rt, srv.URL, "", "")
	assert.Equal(t, "Bearer bearer-123", got.Get("Authorization"))
}

func TestMCPTransport_HeaderMode(t *testing.T) {
	var got http.Header
	srv := captureServer(t, &got)

	rt, err := mcpTransport(MCPSpec{
		Name:            "mcp",
		AuthType:        "header",
		AuthHeaderName:  "X-API-Key",
		AuthHeaderValue: "secret-99",
		BaseTransport:   http.DefaultTransport,
	})
	require.NoError(t, err)
	roundTripAndAssertHeader(t, rt, srv.URL, "", "")
	assert.Equal(t, "secret-99", got.Get("X-API-Key"))
	assert.Empty(t, got.Get("Authorization"))
}

func TestMCPTransport_HeaderMissingFields(t *testing.T) {
	_, err := mcpTransport(MCPSpec{
		Name:          "mcp",
		AuthType:      "header",
		BaseTransport: http.DefaultTransport,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth_header_name")
}

func TestMCPTransport_AutoMode(t *testing.T) {
	var got http.Header
	srv := captureServer(t, &got)

	rt, err := mcpTransport(MCPSpec{
		Name:          "mcp",
		AuthType:      "auto",
		BaseTransport: http.DefaultTransport,
	})
	require.NoError(t, err)
	roundTripAndAssertHeader(t, rt, srv.URL, "", "")
	assert.Empty(t, got.Get("Authorization"))
	assert.Empty(t, got.Get("X-API-Key"))
}

func TestMCPTransport_BackCompatEmptyDefaultsToHugrWhenAuthSet(t *testing.T) {
	var got http.Header
	srv := captureServer(t, &got)

	rt, err := mcpTransport(MCPSpec{
		Name:          "mcp",
		Auth:          "hugr",
		AuthStores:    map[string]auth.TokenStore{"hugr": stubStore{tok: "bc-token"}},
		BaseTransport: http.DefaultTransport,
	})
	require.NoError(t, err)
	roundTripAndAssertHeader(t, rt, srv.URL, "", "")
	assert.Equal(t, "Bearer bc-token", got.Get("Authorization"))
}

func TestMCPTransport_UnknownType(t *testing.T) {
	_, err := mcpTransport(MCPSpec{
		Name:          "mcp",
		AuthType:      "ouch",
		BaseTransport: http.DefaultTransport,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown auth_type")
}
