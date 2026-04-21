package tools

import "time"

// MCPConfig holds MCP-wide behaviour knobs shared by every MCP-backed
// tools.Provider. Loaded from config.yaml `mcp:` block.
type MCPConfig struct {
	// TTL is how long a provider keeps the ListTools result cached between
	// explicit invalidation events. Zero = no TTL (always refetch).
	TTL time.Duration `mapstructure:"ttl"`
	// FetchTimeout bounds a single ListTools call.
	FetchTimeout time.Duration `mapstructure:"fetch_timeout"`
}

// ProviderConfig declares a tools.Provider instance built at startup.
// Only `type: mcp` is supported currently — internal services (_skills,
// _memory, _context) self-register in cmd/agent/runtime.go.
//
// For type=mcp the `transport` field selects the MCP transport:
//   - "streamable-http" (default): Streamable HTTP (one URL, one HTTP
//     client, bidirectional chunked JSON-RPC).
//   - "sse": Server-Sent Events over HTTP.
//   - "stdio": spawns `command` + `args` and talks JSON-RPC over
//     stdin/stdout. Auth-by-name is ignored; credentials go through
//     env instead.
type ProviderConfig struct {
	Name      string            `mapstructure:"name"`
	Type      string            `mapstructure:"type"`      // mcp
	Transport string            `mapstructure:"transport"` // streamable-http|sse|stdio
	Endpoint  string            `mapstructure:"endpoint"`  // for HTTP transports
	Command   string            `mapstructure:"command"`   // for stdio
	Args      []string          `mapstructure:"args"`      // for stdio
	Env       map[string]string `mapstructure:"env"`       // for stdio
	Auth      string            `mapstructure:"auth"`      // optional auth config name (HTTP only)
}
