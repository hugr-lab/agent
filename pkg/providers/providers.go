// Package providers is the central type registry for tools.Provider
// builders driven by config.yaml. Each provider entry in
// config.Providers has a `type:` field; the registry looks up a
// Builder for that type and constructs the provider with shared Deps.
//
// Built-in types: `mcp` (MCP endpoint) and `system` (agent internal
// tool suites). New types are registered via Register — typically in
// an init() alongside their builder.
package providers

import (
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/sessions"
	"github.com/hugr-lab/hugen/pkg/skills"
	"github.com/hugr-lab/hugen/pkg/store"
	"github.com/hugr-lab/hugen/pkg/tools"
	"github.com/hugr-lab/hugen/pkg/tools/system"
)

// Builder constructs a tools.Provider from a ProviderConfig and the
// shared runtime Deps. It must not panic on misconfigured input —
// return an error instead.
type Builder func(cfg config.ProviderConfig, deps Deps) (tools.Provider, error)

// Deps is the shared runtime handed to every builder. Builders pick
// what they need and ignore the rest.
type Deps struct {
	// AuthStores is the name→TokenStore map from auth.BuildStores.
	AuthStores    map[string]auth.TokenStore
	BaseTransport http.RoundTripper

	// For system-type builders:
	Sessions *sessions.Manager
	Skills   skills.Manager
	Hub      store.DB

	// Compactor is the on-demand context compressor used by the
	// `_context` system suite's context_compress tool. May be nil;
	// the tool then degrades to a no-op with an informative reason.
	Compactor system.OnDemandCompactor

	// MCP defaults propagated into every MCP provider (TTL + timeout).
	MCP config.MCPConfig

	Logger *slog.Logger
}

var (
	mu       sync.RWMutex
	builders = map[string]Builder{}
)

// Register adds / replaces a builder for the given type. Call from
// init() in type-specific files (mcp.go, system.go).
func Register(typ string, b Builder) {
	if typ == "" || b == nil {
		panic("providers.Register: empty type or nil builder")
	}
	mu.Lock()
	defer mu.Unlock()
	builders[typ] = b
}

// Build looks up a builder by cfg.Type and invokes it.
func Build(cfg config.ProviderConfig, deps Deps) (tools.Provider, error) {
	mu.RLock()
	b, ok := builders[cfg.Type]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("providers: unknown type %q for provider %q", cfg.Type, cfg.Name)
	}
	return b(cfg, deps)
}

// RegisteredTypes lists known builder keys — debug helper for logs /
// admin endpoints.
func RegisteredTypes() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(builders))
	for k := range builders {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// BuildAll walks cfg entries in order, invokes Build for each, and
// registers the result in tm. Failure on any entry aborts — partial
// registration is worse than a startup error the operator can see.
func BuildAll(cfgs []config.ProviderConfig, tm *tools.Manager, deps Deps) error {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	for _, c := range cfgs {
		p, err := Build(c, deps)
		if err != nil {
			return fmt.Errorf("providers.BuildAll: %q: %w", c.Name, err)
		}
		if p.Name() != c.Name {
			return fmt.Errorf("providers.BuildAll: %q built provider with name %q — mismatch", c.Name, p.Name())
		}
		tm.AddProvider(p)
		deps.Logger.Info("provider registered", "name", c.Name, "type", c.Type)
	}
	return nil
}
