package embedding

import (
	"context"
	"fmt"

	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/pkg/store/queries"
)

// Embed returns a vector for text via the configured embedding data
// source (core.models.embedding). Returns ErrDisabled when no model
// is configured — callers should fall back to FTS in that case.
//
// Vector arrives wire-encoded as a quoted string ("[0.1, 0.2, ...]")
// because types.Vector has a custom MarshalJSON; its matching
// UnmarshalJSON decodes that back into []float64 on this side.
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	if !c.Available() {
		return nil, ErrDisabled
	}
	type result struct {
		Vector types.Vector `json:"vector"`
	}
	r, err := queries.RunQuery[result](ctx, c.querier,
		`query ($model: String!, $input: String!) {
			function { core { models { embedding(model: $model, input: $input) {
				vector
			} } } }
		}`,
		map[string]any{"model": c.model, "input": text},
		"function.core.models.embedding",
	)
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	out := make([]float32, len(r.Vector))
	for i, v := range r.Vector {
		out[i] = float32(v)
	}
	return out, nil
}

// EmbedBatch returns vectors for texts, one call per text. Providers
// that expose batch endpoints can be plugged in later without changing
// callers.
func (c *Client) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if !c.Available() {
		return nil, ErrDisabled
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, err := c.Embed(ctx, t)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}
