package auth

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// AuthSpec is the transport-agnostic input to BuildStores: one entry
// per named auth config in config.yaml. Callers translate their own
// config type (e.g. internal/config.AuthConfig) into this shape so
// pkg/auth stays free of project-specific imports.
type AuthSpec struct {
	Name         string
	Type         string // hugr | oidc
	Issuer       string
	ClientID     string
	CallbackPath string
	BaseURL      string // e.g. http://localhost:10000 — used to build RedirectURL when OIDC path taken
	AccessToken  string
	TokenURL     string
	// DiscoverURL is the hugr URL used for type=hugr when no
	// access_token/token_url is set: BuildStores calls
	// {DiscoverURL}/auth/config to fetch issuer + client_id.
	DiscoverURL string
}

// Stores is the result of BuildStores — a name→TokenStore map plus
// any post-listener hooks (OIDC PromptLogin) to invoke after the
// HTTP listener is bound.
type Stores struct {
	Tokens      map[string]TokenStore
	PromptLogin []func()
}

// BuildStores walks specs and creates a TokenStore for each. OIDC
// entries are registered on mux at their configured CallbackPath.
// Returns an error at first collision / discovery failure.
//
// Callers compose the returned stores with HTTP transports using
// Transport(store, base) or a project-specific HeaderTransportFactory
// for secret-key auth. BaseURL on each spec is used to derive the
// OIDC RedirectURL when the spec didn't set one explicitly.
func BuildStores(ctx context.Context, specs []AuthSpec, mux *http.ServeMux, logger *slog.Logger) (*Stores, error) {
	if logger == nil {
		logger = slog.Default()
	}
	out := &Stores{
		Tokens: make(map[string]TokenStore, len(specs)),
	}
	seenPaths := map[string]string{}

	for _, s := range specs {
		switch s.Type {
		case "hugr":
			if err := buildHugrAuth(ctx, s, out, mux, seenPaths, logger); err != nil {
				return nil, err
			}

		case "oidc":
			if err := buildOIDCAuth(ctx, s, out, mux, seenPaths, logger); err != nil {
				return nil, err
			}

		default:
			return nil, fmt.Errorf("auth %q: unsupported type %q (want hugr|oidc)", s.Name, s.Type)
		}
	}
	return out, nil
}

// buildHugrAuth is the "smart" composite used for every hugr-facing
// connection. Priority:
//
//  1. access_token + token_url → RemoteStore (production; hub
//     pre-authenticated the user, no browser flow).
//  2. fallback: discover issuer + client_id via {DiscoverURL}/auth/config
//     and start an OIDC browser flow. Requires DiscoverURL to be set.
func buildHugrAuth(ctx context.Context, s AuthSpec, out *Stores, mux *http.ServeMux, seenPaths map[string]string, logger *slog.Logger) error {
	if s.AccessToken != "" && s.TokenURL != "" {
		out.Tokens[s.Name] = NewRemoteStore(s.AccessToken, s.TokenURL)
		logger.Info("auth store built",
			"name", s.Name, "type", "hugr", "mode", "token", "token_url", s.TokenURL)
		return nil
	}

	// OIDC fallback — need a hugr URL to discover from.
	issuer := s.Issuer
	clientID := s.ClientID
	if (issuer == "" || clientID == "") && s.DiscoverURL != "" {
		disc, err := DiscoverOIDCFromHugr(ctx, s.DiscoverURL)
		if err != nil {
			return fmt.Errorf("auth %q: discover from %s: %w", s.Name, s.DiscoverURL, err)
		}
		if disc == nil {
			return fmt.Errorf("auth %q: discover from %s returned empty config", s.Name, s.DiscoverURL)
		}
		if issuer == "" {
			issuer = disc.Issuer
		}
		if clientID == "" {
			clientID = disc.ClientID
		}
	}
	if issuer == "" || clientID == "" {
		return fmt.Errorf("auth %q: no token_url/access_token and discovery did not yield issuer+client_id", s.Name)
	}

	return finalizeOIDC(ctx, s, issuer, clientID, out, mux, seenPaths, logger, "hugr")
}

// buildOIDCAuth is a plain OIDC config — expects explicit issuer +
// client_id, no hugr discovery. For third-party IdPs.
func buildOIDCAuth(ctx context.Context, s AuthSpec, out *Stores, mux *http.ServeMux, seenPaths map[string]string, logger *slog.Logger) error {
	if s.Issuer == "" || s.ClientID == "" {
		return fmt.Errorf("auth %q: type=oidc needs issuer + client_id", s.Name)
	}
	return finalizeOIDC(ctx, s, s.Issuer, s.ClientID, out, mux, seenPaths, logger, "oidc")
}

// finalizeOIDC builds the OIDCStore, registers the callback route on
// mux (enforcing path uniqueness), stores the result, and queues the
// PromptLogin trigger.
func finalizeOIDC(ctx context.Context, s AuthSpec, issuer, clientID string, out *Stores, mux *http.ServeMux, seenPaths map[string]string, logger *slog.Logger, logType string) error {
	path := s.CallbackPath
	if path == "" {
		path = "/auth/callback"
	}
	if owner, dup := seenPaths[path]; dup {
		return fmt.Errorf("auth: callback path %q conflicts between %q and %q", path, owner, s.Name)
	}
	seenPaths[path] = s.Name

	redirect := strings.TrimRight(s.BaseURL, "/") + path
	cfg := OIDCConfig{
		IssuerURL:    issuer,
		ClientID:     clientID,
		RedirectURL:  redirect,
		CallbackPath: path,
		LoginPath:    strings.Replace(path, "callback", "login", 1),
		Logger:       logger.With("auth", s.Name),
	}
	store, err := NewOIDCStore(ctx, cfg)
	if err != nil {
		return fmt.Errorf("auth %q: %w", s.Name, err)
	}
	if mux != nil {
		store.RegisterCallbackRoute(mux)
	}
	out.Tokens[s.Name] = store
	out.PromptLogin = append(out.PromptLogin, store.PromptLogin)
	logger.Info("auth store built",
		"name", s.Name, "type", logType, "mode", "oidc", "callback", path, "issuer", issuer)
	return nil
}
