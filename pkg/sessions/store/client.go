// Package store exposes agent-scoped hub.db session operations
// (sessions + session_events + session_notes + session_participants)
// through a typed Client. Constructed by the caller with a
// types.Querier.
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
	// EmbedderEnabled toggles the `summary:` argument on
	// `insert_session_events`. The schema only exposes the argument
	// when the `@embeddings` directive is applied, which requires an
	// embedder data source to be wired in the engine. Tests that spin
	// up an engine without one leave this false so the classifier's
	// writes still land with NULL embedding.
	EmbedderEnabled bool
}

// Client is the agent-scoped hub.db sessions API.
type Client struct {
	querier         types.Querier
	agentID         string
	agentShort      string
	logger          *slog.Logger
	embedderEnabled bool
}

// New constructs the Client.
func New(querier types.Querier, opts Options) (*Client, error) {
	if querier == nil {
		return nil, fmt.Errorf("sessions: nil querier")
	}
	if opts.AgentID == "" {
		return nil, fmt.Errorf("sessions: AgentID required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Client{
		querier:         querier,
		agentID:         opts.AgentID,
		agentShort:      opts.AgentShort,
		logger:          opts.Logger,
		embedderEnabled: opts.EmbedderEnabled,
	}, nil
}

// AgentID returns the scope the Client was constructed for.
func (c *Client) AgentID() string { return c.agentID }
