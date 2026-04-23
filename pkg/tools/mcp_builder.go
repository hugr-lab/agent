package tools

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/hugr-lab/hugen/pkg/auth"
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

	// Auth is an optional auth-config name. When set and AuthType
	// resolves to "hugr", the named store's RoundTripper wraps the
	// base transport for HTTP transports. Ignored for stdio.
	Auth       string
	AuthStores map[string]auth.TokenStore

	// AuthType selects the HTTP transport wrapping.
	//   - "hugr" / ""  → Bearer token from AuthStores[Auth] (back-compat).
	//   - "header"     → inject AuthHeaderName: AuthHeaderValue
	//   - "auto"       → no wrap (MCP server handles auth)
	AuthType        string
	AuthHeaderName  string
	AuthHeaderValue string

	// BaseTransport is the default (unauthenticated) RoundTripper.
	BaseTransport http.RoundTripper

	// Config carries MCP-wide TTL + fetch timeout.
	Config MCPConfig
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
// use based on spec.AuthType. Empty AuthType falls back to "hugr"
// when Auth is set (back-compat), else "auto".
func mcpTransport(spec MCPSpec) (http.RoundTripper, error) {
	base := spec.BaseTransport
	if base == nil {
		base = http.DefaultTransport
	}

	kind := strings.ToLower(spec.AuthType)
	if kind == "" {
		if spec.Auth != "" {
			kind = "hugr"
		} else {
			kind = "auto"
		}
	}

	switch kind {
	case "hugr":
		if spec.Auth == "" {
			// No store to pull from — behave like auto for
			// anonymous endpoints.
			return base, nil
		}
		store, ok := spec.AuthStores[spec.Auth]
		if !ok || store == nil {
			return nil, fmt.Errorf("auth %q not found in store registry", spec.Auth)
		}
		return auth.Transport(store, base), nil

	case "header":
		if spec.AuthHeaderName == "" || spec.AuthHeaderValue == "" {
			return nil, fmt.Errorf("provider %q: auth_type=header requires auth_header_name + auth_header_value", spec.Name)
		}
		return auth.HeaderTransport(spec.AuthHeaderName, spec.AuthHeaderValue, base), nil

	case "auto":
		return base, nil

	default:
		return nil, fmt.Errorf("provider %q: unknown auth_type %q (want hugr|header|auto)", spec.Name, spec.AuthType)
	}
}
