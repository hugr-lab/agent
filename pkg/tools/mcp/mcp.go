// Package mcp wires an MCP endpoint into a tools.Provider. Providers
// are built lazily (on first skill_load) and support streamable-http,
// SSE, and stdio transports. Tools() is TTL-cached; the cache is also
// dropped on explicit Invalidate() and on an MCP
// "notifications/tools/list_changed" push from the server.
package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"sync"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/agent"
	adksession "google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/mcptoolset"
	"google.golang.org/genai"
)

// defaultFetchTimeout bounds each ListTools call so a misbehaving or
// unauthorised MCP endpoint can't hang the agent.
const defaultFetchTimeout = 30 * time.Second

// Transport selects the MCP wire protocol. Only "streamable-http",
// "sse", and "stdio" are recognised; any other value returns an error
// from New.
type Transport string

const (
	TransportStreamableHTTP Transport = "streamable-http"
	TransportSSE            Transport = "sse"
	TransportStdio          Transport = "stdio"
)

// Options tunes Provider behaviour. Exactly which fields matter
// depends on TransportType:
//
//   - streamable-http / sse: Endpoint + HTTPTransport (bearer-injected
//     round-tripper from pkg/auth, or plain http.DefaultTransport).
//   - stdio: Command + Args + Env. HTTPTransport is unused.
type Options struct {
	TransportType Transport

	// HTTP transports.
	Endpoint      string
	HTTPTransport http.RoundTripper

	// Stdio transport.
	Command string
	Args    []string
	Env     map[string]string

	// Shared.
	TTL           time.Duration
	FetchTimeout  time.Duration
	Logger        *slog.Logger
	ClientName    string
	ClientVersion string
}

// Provider is a tools.Provider backed by an MCP endpoint.
// Implements tools.CacheableProvider.
type Provider struct {
	name      string
	transport Transport
	endpoint  string // empty for stdio
	toolset   tool.Toolset
	ttl       time.Duration
	timeout   time.Duration
	logger    *slog.Logger

	mu      sync.Mutex
	cache   []tool.Tool
	fetched time.Time
	lastErr error
}

// New wires an MCP endpoint into a Provider. The MCP session is created
// lazily on the first Tools() call; notifications/tools/list_changed
// from the server drop the cache so the next call refetches.
func New(name string, opts Options) (*Provider, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if opts.FetchTimeout <= 0 {
		opts.FetchTimeout = defaultFetchTimeout
	}
	if opts.ClientName == "" {
		opts.ClientName = "hugr-agent-mcp"
	}
	if opts.ClientVersion == "" {
		opts.ClientVersion = "0"
	}
	if opts.TransportType == "" {
		opts.TransportType = TransportStreamableHTTP
	}

	p := &Provider{
		name:      name,
		transport: opts.TransportType,
		endpoint:  opts.Endpoint,
		ttl:       opts.TTL,
		timeout:   opts.FetchTimeout,
		logger:    logger,
	}

	transport, err := buildSDKTransport(name, opts)
	if err != nil {
		return nil, err
	}

	client := sdkmcp.NewClient(
		&sdkmcp.Implementation{Name: opts.ClientName, Version: opts.ClientVersion},
		&sdkmcp.ClientOptions{
			ToolListChangedHandler: func(_ context.Context, _ *sdkmcp.ToolListChangedRequest) {
				logger.Info("mcp: tool list changed push received", "provider", name)
				p.Invalidate()
			},
		},
	)
	ts, err := mcptoolset.New(mcptoolset.Config{
		Client:    client,
		Transport: transport,
	})
	if err != nil {
		return nil, fmt.Errorf("mcp: new toolset %q: %w", name, err)
	}
	p.toolset = ts
	return p, nil
}

// buildSDKTransport dispatches on Options.TransportType to construct
// the appropriate go-sdk transport.
func buildSDKTransport(name string, opts Options) (sdkmcp.Transport, error) {
	switch opts.TransportType {
	case TransportStreamableHTTP:
		if opts.Endpoint == "" {
			return nil, fmt.Errorf("mcp: %q streamable-http needs endpoint", name)
		}
		httpClient := &http.Client{}
		if opts.HTTPTransport != nil {
			httpClient.Transport = opts.HTTPTransport
		}
		return &sdkmcp.StreamableClientTransport{
			Endpoint:             opts.Endpoint,
			DisableStandaloneSSE: true,
			HTTPClient:           httpClient,
		}, nil

	case TransportSSE:
		if opts.Endpoint == "" {
			return nil, fmt.Errorf("mcp: %q sse needs endpoint", name)
		}
		httpClient := &http.Client{}
		if opts.HTTPTransport != nil {
			httpClient.Transport = opts.HTTPTransport
		}
		return &sdkmcp.SSEClientTransport{
			Endpoint:   opts.Endpoint,
			HTTPClient: httpClient,
		}, nil

	case TransportStdio:
		if opts.Command == "" {
			return nil, fmt.Errorf("mcp: %q stdio needs command", name)
		}
		cmd := exec.Command(opts.Command, opts.Args...)
		if len(opts.Env) > 0 {
			// Start from process environment and overlay provider env.
			env := cmd.Env
			for k, v := range opts.Env {
				env = append(env, k+"="+v)
			}
			cmd.Env = env
		}
		return &sdkmcp.CommandTransport{Command: cmd}, nil

	default:
		return nil, fmt.Errorf("mcp: %q unsupported transport %q (want streamable-http|sse|stdio)", name, opts.TransportType)
	}
}

// Name returns the provider's unique key (typically the skill name or
// a configured provider name).
func (p *Provider) Name() string { return p.name }

// Endpoint returns the configured MCP endpoint URL (empty for stdio).
func (p *Provider) Endpoint() string { return p.endpoint }

// TransportType returns the wire protocol this provider uses.
func (p *Provider) TransportType() Transport { return p.transport }

// Tools returns the cached tool list, refreshing when the TTL window
// expires. On fetch error the previous cached list is kept (if any) —
// a transient network blip shouldn't delete tools from a running
// session.
func (p *Provider) Tools() []tool.Tool {
	p.mu.Lock()
	fresh := p.ttl > 0 && !p.fetched.IsZero() && time.Since(p.fetched) < p.ttl
	if fresh {
		cached := p.cache
		p.mu.Unlock()
		return cached
	}
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	ts, err := p.toolset.Tools(readonlyCtx{Context: ctx})

	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.lastErr = err
		p.logger.Error("mcp: fetch tools", "provider", p.name, "transport", p.transport, "endpoint", p.endpoint, "err", err)
		return p.cache
	}
	p.cache = ts
	p.fetched = time.Now()
	p.lastErr = nil
	p.logger.Debug("mcp: tools fetched", "provider", p.name, "count", len(ts))
	return p.cache
}

// Invalidate drops the cached tool list. Next Tools() call refetches.
func (p *Provider) Invalidate() {
	p.mu.Lock()
	p.fetched = time.Time{}
	p.mu.Unlock()
}

// readonlyCtx is a no-op agent.ReadonlyContext; mcptoolset's Tools()
// only uses the embedded context.Context for deadline propagation.
type readonlyCtx struct {
	context.Context
}

var _ agent.ReadonlyContext = readonlyCtx{}

func (readonlyCtx) UserContent() *genai.Content             { return nil }
func (readonlyCtx) InvocationID() string                    { return "" }
func (readonlyCtx) AgentName() string                       { return "" }
func (readonlyCtx) ReadonlyState() adksession.ReadonlyState { return nil }
func (readonlyCtx) UserID() string                          { return "" }
func (readonlyCtx) AppName() string                         { return "" }
func (readonlyCtx) SessionID() string                       { return "" }
func (readonlyCtx) Branch() string                          { return "" }
