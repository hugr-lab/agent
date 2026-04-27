package runtime

import (
	"context"
	"fmt"
	"log/slog"

	qe "github.com/hugr-lab/query-engine"
	qetypes "github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/pkg/models"
	"github.com/hugr-lab/hugen/pkg/store/local"
)

// BuildLocalEngine spins up the embedded query-engine backed by
// cfg.LocalDB and returns it along with the LLM model names declared
// in cfg.LocalDB.Models that the router should route to it.
//
// Returns (nil, nil, nil) when cfg.LocalDBEnabled is false — callers
// can use this without first checking the flag.
func BuildLocalEngine(ctx context.Context, cfg *RuntimeConfig, logger *slog.Logger) (*qe.Service, []string, error) {
	if !cfg.LocalDBEnabled {
		return nil, nil, nil
	}
	engine, err := local.New(ctx, cfg.LocalDB, cfg.Identity, cfg.Embedding, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("runtime: local engine: %w", err)
	}
	ms := make([]string, 0, len(cfg.LocalDB.Models))
	for _, m := range cfg.LocalDB.Models {
		if m.Type == "embedding" {
			continue
		}
		ms = append(ms, m.Name)
	}
	return engine, ms, nil
}

// BuildRouter wires a models.Router around the local + remote
// queriers. localQuerier may be nil (hub-only mode); remoteQuerier
// falls back to localQuerier when nil — the router contract requires
// a non-nil remote slot. localModels names the data sources that live
// inside the local engine; the router uses this to decide which slot
// answers each model name.
func BuildRouter(localQuerier, remoteQuerier qetypes.Querier, localModels []string, llmCfg models.Config, logger *slog.Logger) *models.Router {
	if remoteQuerier == nil {
		remoteQuerier = localQuerier
	}
	return models.NewRouter(
		localQuerier,
		remoteQuerier,
		localModels,
		llmCfg,
		models.WithLogger(logger),
		models.WithToolChoiceFunc(func() string { return "auto" }),
	).WithLogger(logger)
}
