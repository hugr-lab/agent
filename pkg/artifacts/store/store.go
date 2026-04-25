// Package store exposes agent-scoped hub.db artifact registry
// operations (artifacts + artifact_grants) through a typed Client.
// Constructed by pkg/artifacts.New with a types.Querier (embedded
// engine in local mode, *client.Client in remote mode); the Client
// does not know which.
//
// Mirrors the layout of pkg/sessions/store and pkg/memory/store so
// that anyone navigating from one package to another finds the same
// shape: stateless typed Client wrapping types.Querier, value-type
// Records on the boundary, GraphQL mutations on the inside.
package store

import (
	"fmt"
	"log/slog"

	"github.com/hugr-lab/query-engine/types"
)

// Options configures the Client. AgentID is required; AgentShort is
// the short identifier used by id.New for synthesised artifact ids.
// EmbedderEnabled toggles whether Insert passes `summary: <description>`
// to insert_artifacts so Hugr embeds server-side. True in production
// (embedder is a hard runtime dep per spec 006); false in tests
// against a hugr engine without an attached embedder.
type Options struct {
	AgentID         string
	AgentShort      string
	Logger          *slog.Logger
	EmbedderEnabled bool
}

// Client is the agent-scoped hub.db artifact registry API.
// Stateless: every method hits the wire without per-artifact
// bookkeeping.
type Client struct {
	querier         types.Querier
	agentID         string
	agentShort      string
	logger          *slog.Logger
	embedderEnabled bool
}

// New constructs the Client. Returns an error when querier is nil or
// AgentID is empty. Logger defaults to slog.Default.
func New(querier types.Querier, opts Options) (*Client, error) {
	if querier == nil {
		return nil, fmt.Errorf("artifacts/store: nil querier")
	}
	if opts.AgentID == "" {
		return nil, fmt.Errorf("artifacts/store: AgentID required")
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

// AgentShort returns the agent's short identifier (used by id.New).
func (c *Client) AgentShort() string { return c.agentShort }

// EmbedderEnabled reports whether the Client was wired with the
// server-side embedder. ListVisible's semantic-search route gates
// on this — when false the manager falls back to plain ranking.
func (c *Client) EmbedderEnabled() bool { return c.embedderEnabled }
