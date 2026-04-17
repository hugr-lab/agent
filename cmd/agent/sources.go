package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	hugr "github.com/hugr-lab/query-engine"
	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/internal/config"
)

// registerModelSources registers each cfg.Models entry whose type matches one
// of the predicate-accepted types as a data source in the engine and loads it.
// ${ENV_VAR} in Path is expanded at registration time.
func registerModelSources(ctx context.Context, engine *hugr.Service, models []config.ModelDef, typeMatches func(string) bool, logger *slog.Logger) error {
	for _, m := range models {
		if !typeMatches(m.Type) {
			continue
		}
		ds := types.DataSource{
			Name: m.Name,
			Type: types.DataSourceType(m.Type),
			Path: os.ExpandEnv(m.Path),
		}
		if err := engine.RegisterDataSource(ctx, ds); err != nil {
			return fmt.Errorf("register %s (%s): %w", m.Name, m.Type, err)
		}
		logger.Info("data source registered", "name", m.Name, "type", m.Type)
	}
	return nil
}

func isLLMType(t string) bool      { return strings.HasPrefix(t, "llm-") }
func isEmbeddingType(t string) bool { return t == "embedding" }

// verifyEmbedding runs a probe embedding call against the configured model
// and returns the observed vector length. Used to detect dimension drift
// between config and provider before any memory_item is written.
func verifyEmbedding(ctx context.Context, engine *hugr.Service, model string) (int, error) {
	resp, err := engine.Query(ctx,
		`query ($model: String!) {
			function { core { models { embedding(model: $model, input: "test") {
				vector
			} } } }
		}`,
		map[string]any{"model": model},
	)
	if err != nil {
		return 0, fmt.Errorf("embedding probe: %w", err)
	}
	defer resp.Close()
	if err := resp.Err(); err != nil {
		return 0, fmt.Errorf("embedding graphql: %w", err)
	}
	var result struct {
		Vector []float64 `json:"vector"`
	}
	if err := resp.ScanData("function.core.models.embedding", &result); err != nil {
		return 0, fmt.Errorf("embedding scan: %w", err)
	}
	return len(result.Vector), nil
}

// setupLocalSources registers LLM and/or embedding data sources in the
// embedded engine per cfg, applying the failure policy:
//   - Local LLM failure: fatal when no remote Hugr fallback, warn otherwise.
//   - Embedding failure: always warn + FTS fallback (Available=false).
//   - Embedding dimension mismatch vs config: fatal (would silently corrupt
//     stored vectors).
func setupLocalSources(ctx context.Context, engine *hugr.Service, cfg *config.Config, logger *slog.Logger) error {
	if cfg.LLM.Mode == "local" {
		if err := registerModelSources(ctx, engine, cfg.Models, isLLMType, logger); err != nil {
			if cfg.Hugr.URL == "" {
				return fmt.Errorf("llm local mode: %w", err)
			}
			logger.Warn("llm local registration failed — falling back to remote Hugr", "err", err)
		}
	}

	if cfg.Embedding.Mode != "local" || cfg.Embedding.Model == "" {
		return nil
	}
	if err := registerModelSources(ctx, engine, cfg.Models, isEmbeddingType, logger); err != nil {
		logger.Warn("embedding registration failed — FTS fallback", "err", err)
		return nil
	}
	dim, err := verifyEmbedding(ctx, engine, cfg.Embedding.Model)
	if err != nil {
		logger.Warn("embedding probe failed — FTS fallback", "err", err)
		return nil
	}
	if dim != cfg.Embedding.Dimension {
		return fmt.Errorf("embedding dimension mismatch: config=%d provider=%d (model=%s). Update cfg.Embedding.Dimension or recreate the agent",
			cfg.Embedding.Dimension, dim, cfg.Embedding.Model)
	}
	logger.Info("embedding verified", "model", cfg.Embedding.Model, "dimension", dim)
	return nil
}

