package main

import (
	"fmt"
	"log/slog"

	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/adapters/hubdb"
	"github.com/hugr-lab/hugen/interfaces"
	"github.com/hugr-lab/hugen/pkg/config"
)

// buildHubDB constructs the HubDB over the provided querier (embedded engine
// or remote client). agentID/shortID come from config.Agent identity.
func buildHubDB(cfg *config.Config, querier types.Querier, logger *slog.Logger) (interfaces.HubDB, error) {
	if cfg.Identity.ID == "" {
		return nil, fmt.Errorf("config: agent.id is required")
	}
	return hubdb.New(querier, hubdb.Options{
		AgentID:        cfg.Identity.ID,
		AgentShort:     cfg.Identity.ShortID,
		Dimension:      cfg.Embedding.Dimension,
		EmbeddingModel: cfg.Embedding.Model,
		Logger:         logger,
	})
}
