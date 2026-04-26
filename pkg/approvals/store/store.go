// Package store exposes agent-scoped hub.db approvals + tool_policies
// CRUD through a typed Client. Constructed by pkg/approvals.New with
// a types.Querier (embedded engine in local mode, *client.Client in
// remote mode); the Client does not know which.
//
// Mirrors pkg/sessions/store, pkg/memory/store, pkg/artifacts/store
// so anyone navigating between packages finds the same shape:
// stateless typed Client wrapping types.Querier, value-type Records
// on the boundary, GraphQL mutations on the inside.
package store

import (
	"fmt"
	"log/slog"

	"github.com/hugr-lab/query-engine/types"
)

// Options configures the Client. AgentID is required.
type Options struct {
	AgentID string
	Logger  *slog.Logger
}

// Client is the agent-scoped hub.db approvals + tool_policies API.
// Stateless: every method hits the wire without per-row bookkeeping.
type Client struct {
	querier types.Querier
	agentID string
	logger  *slog.Logger
}

// New constructs the Client. Returns an error when querier is nil or
// AgentID is empty. Logger defaults to slog.Default.
func New(querier types.Querier, opts Options) (*Client, error) {
	if querier == nil {
		return nil, fmt.Errorf("approvals/store: nil querier")
	}
	if opts.AgentID == "" {
		return nil, fmt.Errorf("approvals/store: AgentID required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Client{
		querier: querier,
		agentID: opts.AgentID,
		logger:  opts.Logger,
	}, nil
}

// AgentID returns the scope the Client was constructed for.
func (c *Client) AgentID() string { return c.agentID }
