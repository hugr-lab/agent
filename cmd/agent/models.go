package main

import (
	"log/slog"

	"github.com/hugr-lab/hugen/pkg/models"
	hugenruntime "github.com/hugr-lab/hugen/pkg/runtime"
	qetypes "github.com/hugr-lab/query-engine/types"
)

// buildModelRouter is Phase 8b: assembles the LLM router from the
// resolved local + remote queriers and the cfg.LLM intent routes.
// Pass-through to hugenruntime.BuildRouter — kept as a separate file
// so the bootstrap phases line up one-to-one with the explicit steps
// in main.go.
func buildModelRouter(localQuerier, remoteQuerier qetypes.Querier, localModels []string, cfg *hugenruntime.RuntimeConfig, logger *slog.Logger) *models.Router {
	return hugenruntime.BuildRouter(localQuerier, remoteQuerier, localModels, cfg.LLM, logger)
}
