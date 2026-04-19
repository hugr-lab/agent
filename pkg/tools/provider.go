package tools

import "google.golang.org/adk/tool"

// Provider supplies a named bundle of tools to the Manager. System tools
// and static mock providers implement just this; providers that memoise
// an upstream fetch (MCP, future hub-tools) add CacheableProvider.
type Provider interface {
	Name() string
	Tools() []tool.Tool
}

// CacheableProvider is implemented by providers that cache Tools() and
// can be told to drop the cache on external signal — hub push, MCP
// notifications/tools/list_changed, or an admin refresh action. After
// Invalidate returns, the next Tools() call re-fetches from the origin.
type CacheableProvider interface {
	Provider
	Invalidate()
}
