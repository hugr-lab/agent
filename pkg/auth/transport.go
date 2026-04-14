package auth

import (
	"fmt"
	"log/slog"
	"net/http"
)

// Transport returns an http.RoundTripper that injects a Bearer token
// from the given TokenStore into every outgoing request.
func Transport(store TokenStore, base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &tokenTransport{store: store, base: base}
}

type tokenTransport struct {
	store TokenStore
	base  http.RoundTripper
}

func (t *tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	slog.Debug("auth transport: getting token", "url", req.URL.String())
	token, err := t.store.Token(req.Context())
	if err != nil {
		slog.Error("auth transport: token error", "err", err, "url", req.URL.String())
		return nil, fmt.Errorf("get auth token: %w", err)
	}
	slog.Debug("auth transport: sending request", "url", req.URL.String())
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+token)
	resp, err := t.base.RoundTrip(r)
	if err != nil {
		slog.Error("auth transport: request failed", "err", err, "url", req.URL.String())
		return nil, err
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		slog.Error("hugr auth rejected", "status", resp.StatusCode, "url", req.URL.String())
	}
	return resp, nil
}
