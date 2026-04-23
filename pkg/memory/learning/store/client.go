// Package store exposes agent-scoped hub.db hypotheses +
// session_reviews operations through a typed Client. Memory-audit log
// (memory_log) lives in pkg/memory/store even though the reviewer
// drives writes into it.
package store

import (
	"fmt"
	"log/slog"

	"github.com/hugr-lab/query-engine/types"
)

// Options configures the Client.
type Options struct {
	AgentID    string
	AgentShort string
	Logger     *slog.Logger
}

// Client is the agent-scoped hub.db learning API.
type Client struct {
	querier    types.Querier
	agentID    string
	agentShort string
	logger     *slog.Logger
}

// New constructs the Client.
func New(querier types.Querier, opts Options) (*Client, error) {
	if querier == nil {
		return nil, fmt.Errorf("learning: nil querier")
	}
	if opts.AgentID == "" {
		return nil, fmt.Errorf("learning: AgentID required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Client{
		querier:    querier,
		agentID:    opts.AgentID,
		agentShort: opts.AgentShort,
		logger:     opts.Logger,
	}, nil
}

// AgentID returns the scope the Client was constructed for.
func (c *Client) AgentID() string { return c.agentID }
