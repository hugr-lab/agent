package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeHugr serves /auth/config with {issuer, client_id} and the OIDC
// discovery document at the returned issuer's /.well-known path —
// enough for NewOIDCStore to succeed without hitting the network.
func fakeHugr(t *testing.T) (hugrURL string) {
	t.Helper()

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Discovery doc under the hugr URL itself — issuer = srv URL keeps
	// things contained.
	mux.HandleFunc("/auth/config", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":    srv.URL,
			"client_id": "agent-client",
		})
	})
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"authorization_endpoint": srv.URL + "/authorize",
			"token_endpoint":         srv.URL + "/token",
		})
	})
	return srv.URL
}

func TestBuildStores_Hugr_TokenMode(t *testing.T) {
	mux := http.NewServeMux()
	out, err := BuildStores(context.Background(), []AuthSpec{
		{
			Name:        "primary",
			Type:        "hugr",
			AccessToken: "seed-token",
			TokenURL:    "http://localhost:9999/token-exchange",
		},
	}, mux, nil)
	require.NoError(t, err)
	require.NotNil(t, out.Tokens["primary"])
	// token mode doesn't register a callback handler, no PromptLogin.
	assert.Empty(t, out.PromptLogin)
}

func TestBuildStores_Hugr_OIDCFallbackDiscovery(t *testing.T) {
	hugrURL := fakeHugr(t)
	mux := http.NewServeMux()

	out, err := BuildStores(context.Background(), []AuthSpec{
		{
			Name:         "primary",
			Type:         "hugr",
			DiscoverURL:  hugrURL,
			BaseURL:      "http://localhost:10000",
			CallbackPath: "/auth/callback",
		},
	}, mux, nil)
	require.NoError(t, err)
	require.NotNil(t, out.Tokens["primary"])
	require.Len(t, out.PromptLogin, 1)
	// OIDC path registered /auth/login/<name> and /auth/callback on mux.
	req := httptest.NewRequest(http.MethodGet, "/auth/login/primary", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	// Should redirect to the IdP authorize endpoint, not 404.
	assert.NotEqual(t, http.StatusNotFound, w.Code)
}

func TestBuildStores_CallbackPathCollision(t *testing.T) {
	hugrURL := fakeHugr(t)
	mux := http.NewServeMux()
	_, err := BuildStores(context.Background(), []AuthSpec{
		{
			Name:         "a",
			Type:         "hugr",
			DiscoverURL:  hugrURL,
			BaseURL:      "http://localhost:10000",
			CallbackPath: "/auth/callback",
		},
		{
			Name:         "b",
			Type:         "hugr",
			DiscoverURL:  hugrURL,
			BaseURL:      "http://localhost:10000",
			CallbackPath: "/auth/callback", // same path
		},
	}, mux, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "callback path")
}

func TestBuildStores_UnknownType(t *testing.T) {
	_, err := BuildStores(context.Background(), []AuthSpec{
		{Name: "weird", Type: "nope"},
	}, http.NewServeMux(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported type")
}

// Derivation of login paths moved into OIDCStore.LoginPath — default
// is "/auth/login/<Name>". The separate derivedLoginPath helper
// retired with the single-/auth/callback dispatcher.

func TestBuildHugrSource_TokenMode(t *testing.T) {
	src, err := BuildHugrSource(context.Background(), AuthSpec{
		Name:        "hugr",
		Type:        "hugr",
		AccessToken: "seed",
		TokenURL:    "http://localhost:9999/token-exchange",
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, "hugr", src.Name())
	_, isRemote := src.(*RemoteStore)
	assert.True(t, isRemote, "token mode yields RemoteStore")
	assert.False(t, src.OwnsState("hugr.any"), "RemoteStore never owns state")
}

func TestBuildSources_AliasHugrType(t *testing.T) {
	// Seed registry with a primary hugr Source (RemoteStore).
	reg := NewSourceRegistry(nil)
	primary, err := BuildHugrSource(context.Background(), AuthSpec{
		Name: "hugr", Type: "hugr", AccessToken: "seed", TokenURL: "http://x/token",
	}, nil)
	require.NoError(t, err)
	require.NoError(t, reg.Add(primary))

	// A provider-auth entry of type=hugr should alias.
	require.NoError(t, BuildSources(context.Background(), []AuthSpec{
		{Name: "mcp-inline", Type: "hugr"},
	}, reg, nil))

	got, ok := reg.Source("mcp-inline")
	require.True(t, ok)
	assert.Same(t, primary, got)
}

func TestBuildSources_HugrWithoutPrimary(t *testing.T) {
	reg := NewSourceRegistry(nil)
	err := BuildSources(context.Background(), []AuthSpec{
		{Name: "mcp-inline", Type: "hugr"},
	}, reg, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no hugr Source registered")
}
