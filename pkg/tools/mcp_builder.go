package tools

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/config"
)

// MCPSpec is the configuration an MCP-backed Provider is built from.
// Used by cmd/agent/runtime.go for each entry in config.yaml's
// providers: list and by sessions.Manager for inline skill-declared
// MCP endpoints.
type MCPSpec struct {
	Name      string
	Transport string // streamable-http (default) | sse | stdio
	Endpoint  string // for HTTP transports
	Command   string // for stdio
	Args      []string
	Env       map[string]string

	// Auth is an optional auth-config name. When set, the named store's
	// RoundTripper wraps the base transport for HTTP transports. Ignored
	// for stdio.
	Auth       string
	AuthStores map[string]auth.TokenStore

	// BaseTransport is the default (unauthenticated) RoundTripper.
	BaseTransport http.RoundTripper

	// Config carries MCP-wide TTL + fetch timeout.
	Config config.MCPConfig
	Logger *slog.Logger
}

// NewMCPProvider constructs an MCP-backed Provider from spec. Returns
// an error for unsupported transport or missing auth store.
func NewMCPProvider(spec MCPSpec) (Provider, error) {
	transport := Transport(spec.Transport)
	if transport == "" {
		transport = TransportStreamableHTTP
	}

	opts := Options{
		TransportType: transport,
		Endpoint:      spec.Endpoint,
		Command:       spec.Command,
		Args:          spec.Args,
		Env:           spec.Env,
		TTL:           spec.Config.TTL,
		FetchTimeout:  spec.Config.FetchTimeout,
		Logger:        spec.Logger,
	}

	switch transport {
	case TransportStreamableHTTP, TransportSSE:
		httpTransport, err := mcpTransport(spec)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", spec.Name, err)
		}
		opts.HTTPTransport = httpTransport

	case TransportStdio:
		if spec.Auth != "" && spec.Logger != nil {
			spec.Logger.Warn("provider: auth ignored on stdio transport",
				"provider", spec.Name, "auth", spec.Auth)
		}

	default:
		return nil, fmt.Errorf("provider %q: unsupported transport %q", spec.Name, spec.Transport)
	}

	return newMCP(spec.Name, opts)
}

// mcpTransport resolves the HTTP round-tripper an MCP builder should
// use: token-injected for the named auth store, or the base transport
// when auth is empty.
func mcpTransport(spec MCPSpec) (http.RoundTripper, error) {
	base := spec.BaseTransport
	if base == nil {
		base = http.DefaultTransport
	}
	if spec.Auth == "" {
		return base, nil
	}
	store, ok := spec.AuthStores[spec.Auth]
	if !ok || store == nil {
		return nil, fmt.Errorf("auth %q not found in store registry", spec.Auth)
	}
	return auth.Transport(store, base), nil
}
