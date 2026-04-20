package main

import (
	"fmt"
	"log/slog"

	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/store"
)

// buildHubDB constructs the HubDB over the provided querier (embedded engine
// or remote client). agentID/shortID come from config.Agent identity.
func buildHubDB(cfg *config.Config, querier types.Querier, logger *slog.Logger) (store.DB, error) {
	if cfg.Identity.ID == "" {
		return nil, fmt.Errorf("config: agent.id is required")
	}
	return store.New(querier, store.Options{
		AgentID:        cfg.Identity.ID,
		AgentShort:     cfg.Identity.ShortID,
		Dimension:      cfg.Embedding.Dimension,
		EmbeddingModel: cfg.Embedding.Model,
		Logger:         logger,
	})
}
