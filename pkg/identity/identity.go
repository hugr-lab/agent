// Package identity resolves the running agent's principal + config
// against either the hub (remote) or a local YAML file.
//
// Three implementations:
//
//   - hub: queries the hugr platform via types.Querier — used in
//     remote mode where the hub is the source of truth for the
//     agents row + agent_types defaults.
//   - local: reads a local config.yaml — used in fully-local mode
//     when there is no hub to talk to.
//   - local-with-hub: hybrid — reads YAML for overrides but falls
//     through to a hub Source for permission checks / whoami.
//
// Source is consumed at bootstrap time: the cmd/agent main builds
// one once and feeds it to pkg/config (for the merged Config) and
// to pkg/runtime (for the agent identity that keys hub.db rows).
package identity

import (
	"context"
	"time"
)

// Source is the runtime principal lookup interface. Implementations
// decide how Agent / WhoAmI / Permission resolve — the bootstrap
// code does not care.
type Source interface {
	// Agent returns the running agent instance: id, type, name,
	// status, plus the merged config map (agent_type defaults
	// overlaid by per-agent overrides).
	Agent(ctx context.Context) (Agent, error)

	// WhoAmI returns the authenticated subject behind the current
	// connection. In hub mode the hugr transport injects the
	// bearer token, so the result identifies whatever principal
	// that token represents.
	WhoAmI(ctx context.Context) (WhoAmI, error)

	// Permission returns whether the current principal may access
	// (section, name) — used by tool gating + admin endpoints.
	Permission(ctx context.Context, section, name string) (Permission, error)
}

// Agent is a running agent instance.
type Agent struct {
	ID          string         `json:"id"`
	AgentTypeID string         `json:"agent_type_id"`
	Type        string         `json:"type"`
	ShortID     string         `json:"short_id"`
	Name        string         `json:"name"`
	Status      string         `json:"status"`
	Config      map[string]any `json:"config"`
	CreatedAt   time.Time      `json:"created_at"`
	LastActive  time.Time      `json:"last_active"`
}

// WhoAmI is the minimal subject description returned by the hugr
// auth.me endpoint. The hugr client has already applied the bearer
// token via its transport, so the query resolves against whatever
// principal the token represents.
type WhoAmI struct {
	UserID   string `json:"user_id"`
	UserName string `json:"user_name"`
	Role     string `json:"role"`
}

// Permission mirrors function.core.auth.check_access_info — `data`
// and `filters` are GraphQL JSON columns, kept opaque here.
type Permission struct {
	Enabled bool           `json:"enabled"`
	Data    map[string]any `json:"data"`
	Filters map[string]any `json:"filters"`
}
