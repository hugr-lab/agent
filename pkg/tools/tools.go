// Package tools owns the runtime tool catalogue. A Manager holds named
// Providers (the SessionManager for system tools, one MCPProvider per skill
// endpoint, etc.) and resolves flat tool lists by name.
//
// The LLM-facing wiring (BeforeModelCallback rewriting req.Config.Tools +
// req.Tools) lives in callback.go; MCP-backed provider — in pkg/tools/mcp;
// the system-tool suite — in pkg/tools/system.
package tools

import (
	"fmt"
	"log/slog"
	"sync"

	"google.golang.org/adk/tool"
)


// Manager is the central tool registry. Goroutine-safe.
type Manager struct {
	mu        sync.RWMutex
	providers map[string]Provider
	logger    *slog.Logger
}

// New returns an empty Manager. Logger may be nil.
func New(logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		providers: make(map[string]Provider),
		logger:    logger,
	}
}

// AddProvider registers a provider by its Name(). Replaces any existing
// provider with the same name.
func (m *Manager) AddProvider(p Provider) {
	if p == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.providers[p.Name()] = p
}

// Provider looks up a registered provider by name.
func (m *Manager) Provider(name string) (Provider, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.providers[name]
	if !ok {
		return nil, fmt.Errorf("tools: provider %q not registered", name)
	}
	return p, nil
}

// RemoveProvider unregisters a provider by name. No-op if it wasn't
// registered. Returns an error only if name is empty.
func (m *Manager) RemoveProvider(name string) error {
	if name == "" {
		return fmt.Errorf("tools: RemoveProvider requires a name")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.providers, name)
	return nil
}

// ProviderTools returns the tools contributed by a single named provider.
func (m *Manager) ProviderTools(name string) ([]tool.Tool, error) {
	p, err := m.Provider(name)
	if err != nil {
		return nil, err
	}
	return p.Tools(), nil
}

// InvalidateProvider clears the cache of the named provider if it
// implements CacheableProvider. Returns an error if the name is unknown;
// a plain Provider without caching is a no-op (nil).
func (m *Manager) InvalidateProvider(name string) error {
	p, err := m.Provider(name)
	if err != nil {
		return err
	}
	if cp, ok := p.(CacheableProvider); ok {
		cp.Invalidate()
		m.logger.Info("tools: provider cache invalidated", "provider", name)
	}
	return nil
}

// InvalidateAll calls Invalidate on every registered CacheableProvider.
func (m *Manager) InvalidateAll() {
	m.mu.RLock()
	providers := make([]Provider, 0, len(m.providers))
	for _, p := range m.providers {
		providers = append(providers, p)
	}
	m.mu.RUnlock()
	for _, p := range providers {
		if cp, ok := p.(CacheableProvider); ok {
			cp.Invalidate()
		}
	}
	m.logger.Info("tools: all provider caches invalidated")
}

// ProviderNames returns the list of registered provider names. Useful
// for admin endpoints that want to list refreshable targets.
func (m *Manager) ProviderNames() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.providers))
	for name := range m.providers {
		out = append(out, name)
	}
	return out
}
