package providers

import (
	"fmt"
	"net/http"

	"github.com/hugr-lab/hugen/internal/config"
	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/tools"
	"github.com/hugr-lab/hugen/pkg/tools/mcp"
)

func init() {
	Register("mcp", buildMCP)
}

// buildMCP constructs an MCP provider from config. For HTTP-class
// transports the auth-by-name injection pulls the RoundTripper out
// of deps.AuthStores or deps.SecretKeys; stdio transport ignores auth
// (credentials travel via env instead).
func buildMCP(cfg config.ProviderConfig, deps Deps) (tools.Provider, error) {
	transport := mcp.Transport(cfg.Transport)
	if transport == "" {
		transport = mcp.TransportStreamableHTTP
	}

	opts := mcp.Options{
		TransportType: transport,
		Endpoint:      cfg.Endpoint,
		Command:       cfg.Command,
		Args:          cfg.Args,
		Env:           cfg.Env,
		TTL:           deps.MCP.TTL,
		FetchTimeout:  deps.MCP.FetchTimeout,
		Logger:        deps.Logger,
	}

	switch transport {
	case mcp.TransportStreamableHTTP, mcp.TransportSSE:
		httpTransport, err := transportFor(cfg.Auth, deps)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", cfg.Name, err)
		}
		opts.HTTPTransport = httpTransport

	case mcp.TransportStdio:
		// stdio: no HTTP transport. If user set auth: we log the
		// misconfiguration — it's never applied.
		if cfg.Auth != "" {
			deps.Logger.Warn("provider: auth ignored on stdio transport", "provider", cfg.Name, "auth", cfg.Auth)
		}

	default:
		return nil, fmt.Errorf("provider %q: unsupported transport %q", cfg.Name, cfg.Transport)
	}

	return mcp.New(cfg.Name, opts)
}

// transportFor resolves the HTTP round-tripper an MCP builder should
// use: token-injected for hugr/oidc auth, header-stamped for secret-key
// auth, or the base transport when auth is empty.
func transportFor(authName string, deps Deps) (http.RoundTripper, error) {
	base := deps.BaseTransport
	if base == nil {
		base = http.DefaultTransport
	}
	if authName == "" {
		return base, nil
	}
	if store, ok := deps.AuthStores[authName]; ok && store != nil {
		return auth.Transport(store, base), nil
	}
	if key, ok := deps.SecretKeys[authName]; ok {
		if deps.HeaderAuth == nil {
			return nil, fmt.Errorf("auth %q is secret-key but no HeaderAuth factory configured", authName)
		}
		return deps.HeaderAuth(key, base), nil
	}
	return nil, fmt.Errorf("auth %q not found in store registry", authName)
}
