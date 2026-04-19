package hubdb

import (
	"context"
	"fmt"
)

// Embed returns a vector for text via the configured embedding data source
// (core.models.embedding). Returns ErrEmbeddingDisabled when no model
// is configured — callers should fall back to FTS in that case.
func (h *hubDB) Embed(ctx context.Context, text string) ([]float32, error) {
	if !h.Available() {
		return nil, ErrEmbeddingDisabled
	}
	type result struct {
		Vector []float64 `json:"vector"`
	}
	r, err := runQuery[result](ctx, h.querier,
		`query ($model: String!, $input: String!) {
			function { core { models { embedding(model: $model, input: $input) {
				vector
			} } } }
		}`,
		map[string]any{"model": h.embeddingModel, "input": text},
		"function.core.models.embedding",
	)
	if err != nil {
		return nil, fmt.Errorf("hubdb embed: %w", err)
	}
	out := make([]float32, len(r.Vector))
	for i, v := range r.Vector {
		out[i] = float32(v)
	}
	return out, nil
}

// EmbedBatch returns vectors for texts, one call per text. Providers that
// expose batch endpoints can be plugged in later without changing callers.
func (h *hubDB) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if !h.Available() {
		return nil, ErrEmbeddingDisabled
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, err := h.Embed(ctx, t)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// Dimension is the expected vector length for this HubDB. 0 = disabled.
func (h *hubDB) Dimension() int { return h.dimension }

// Available reports whether an embedding model is configured and the
// dimension is non-zero. Does not probe the provider.
func (h *hubDB) Available() bool {
	return h.embeddingModel != "" && h.dimension > 0
}

// ErrEmbeddingDisabled is returned by Embed/EmbedBatch when no embedding
// model is configured. Callers should fall back to full-text search.
var ErrEmbeddingDisabled = fmt.Errorf("hubdb: embedding disabled (no model configured)")
