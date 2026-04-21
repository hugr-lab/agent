// Package embeddings wraps the embedding data source registered in the
// engine (local: local.New registers it; remote: Hugr side). Exposes
// a typed Client to text → vector translation used by pkg/store/memory
// callers (Search + Store) and memory.Reviewer.
package embeddings

import (
	"fmt"
	"log/slog"

	"github.com/hugr-lab/query-engine/types"
)

// Options configures the Client.
type Options struct {
	// Model is the data-source name registered in the engine. Empty =
	// embeddings disabled; Embed/EmbedBatch return ErrDisabled.
	Model string
	// Dimension is the expected vector length. 0 = disabled.
	Dimension int
	Logger    *slog.Logger
}

// Client wraps the embedding model.
type Client struct {
	querier   types.Querier
	model     string
	dimension int
	logger    *slog.Logger
}

// New constructs the Client. Never returns an error — an empty Model /
// zero Dimension simply means "embeddings disabled" and Available()
// reports false.
func New(querier types.Querier, opts Options) *Client {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Client{
		querier:   querier,
		model:     opts.Model,
		dimension: opts.Dimension,
		logger:    opts.Logger,
	}
}

// Dimension is the expected vector length. 0 = disabled.
func (c *Client) Dimension() int { return c.dimension }

// Available reports whether an embedding model is configured and the
// dimension is non-zero. Does not probe the provider.
func (c *Client) Available() bool { return c.model != "" && c.dimension > 0 }

// ErrDisabled is returned by Embed/EmbedBatch when no embedding model
// is configured. Callers should fall back to full-text search.
var ErrDisabled = fmt.Errorf("embeddings: disabled (no model configured)")
