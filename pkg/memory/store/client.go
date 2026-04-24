// Package store exposes agent-scoped hub.db memory operations
// (memory_items + memory_tags + memory_links + memory_log) through a
// typed Client. Constructed by the caller with a types.Querier —
// which can be an embedded *hugr.Service (local mode) or a remote
// *client.Client (hub mode); the Client does not know which.
package store

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/hugr-lab/query-engine/types"
)

// Options configures the Client. AgentID is required; AgentShort is
// used as a deterministic prefix for generated memory-item IDs.
type Options struct {
	AgentID    string
	AgentShort string
	Logger     *slog.Logger
	// EmbedderEnabled toggles whether Store/Reinforce/Supersede pass
	// `summary: <content>` to `insert_memory_items`. True in production
	// (embedder is a required runtime dependency per spec 006 §5d);
	// false in tests that spin a hugr engine without an attached
	// embedder data source, where the `@embeddings` directive is
	// omitted from the schema and the `summary` argument doesn't
	// exist.
	EmbedderEnabled bool
}

// Client is the agent-scoped hub.db memory API.
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
		return nil, fmt.Errorf("memory: nil querier")
	}
	if opts.AgentID == "" {
		return nil, fmt.Errorf("memory: AgentID required")
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

// sessionIDKey is the context-value key used by WithSessionID /
// sessionIDFrom so memory_log rows can be tied to an active session.
type sessionIDKey struct{}

// WithSessionID annotates ctx with sid so subsequent Store / Reinforce
// / Delete calls record the owning session in memory_log.
func WithSessionID(ctx context.Context, sid string) context.Context {
	return context.WithValue(ctx, sessionIDKey{}, sid)
}

func sessionIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(sessionIDKey{}).(string); ok {
		return v
	}
	return ""
}
