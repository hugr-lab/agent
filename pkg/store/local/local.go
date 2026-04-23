package local

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	hugr "github.com/hugr-lab/query-engine"
	"github.com/hugr-lab/query-engine/pkg/auth"
	coredb "github.com/hugr-lab/query-engine/pkg/data-sources/sources/runtime/core-db"
	"github.com/hugr-lab/query-engine/pkg/db"
	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/pkg/store/local/migrate"
)

// New provisions hub.db, constructs the embedded hugr engine, attaches
// hub.db as the "hub.db" RuntimeSource, registers the engine-local
// data sources (LLM + embedding from cfg.Models), and probes the
// embedding dimension.
//
// Returned *hugr.Service satisfies types.Querier and is passed to
// store.New by the caller. Caller owns Close() on failure beyond the
// first step or after the function returns.
//
// Failure policy:
//   - migrate / engine construction / attach / Init → fatal.
//   - Data source registration failures → warn + continue (routing
//     is the caller's business, not this package's).
//   - Embedding dim mismatch vs embedding.Dimension → fatal (would
//     silently corrupt stored vectors in memory_items).
//   - Embedding probe transport errors → warn + FTS fallback.
func New(
	ctx context.Context,
	cfg Config,
	identity Identity,
	embedding EmbeddingConfig,
	logger *slog.Logger,
) (*hugr.Service, error) {
	if logger == nil {
		logger = slog.Default()
	}

	if err := ensureSchema(cfg, identity, embedding); err != nil {
		return nil, err
	}
	logger.Info("hub.db provisioned",
		"path", cfg.MemoryPath,
		"version", migrate.SchemaVersion,
	)

	service, err := newEngine(cfg, embedding)
	if err != nil {
		return nil, err
	}

	if err := attachHubDB(ctx, service, cfg, embedding); err != nil {
		_ = service.Close()
		return nil, err
	}

	if err := service.Init(ctx); err != nil {
		_ = service.Close()
		return nil, fmt.Errorf("engine.Init: %w", err)
	}
	logger.Info("local engine initialised",
		"core_db", cfg.DB.Path,
		"hub_db", cfg.MemoryPath,
		"vector_size", embedding.Dimension,
	)

	registerModelSources(ctx, service, cfg.Models, logger)

	if err := verifyLocalEmbedding(ctx, service, embedding, logger); err != nil {
		_ = service.Close()
		return nil, err
	}
	return service, nil
}

// ensureSchema runs migrate.Ensure with a seed derived from the agent
// identity. No-op on a DB that is already at the target schema
// version.
func ensureSchema(cfg Config, identity Identity, embedding EmbeddingConfig) error {
	seed := &migrate.SeedData{
		AgentType: migrate.SeedAgentType{
			ID:   identity.Type,
			Name: identity.Type,
		},
		Agent: migrate.SeedAgent{
			ID:      identity.ID,
			ShortID: identity.ShortID,
			Name:    identity.Name,
		},
	}
	if err := migrate.Ensure(migrate.Config{
		Path:          cfg.MemoryPath,
		VectorSize:    embedding.Dimension,
		EmbedderModel: embedding.Model,
		Seed:          seed,
	}); err != nil {
		return fmt.Errorf("migrate hub.db: %w", err)
	}
	return nil
}

// newEngine constructs the embedded hugr engine backed by the
// configured CoreDB and pool settings. Memory hub.db is attached
// separately in attachHubDB.
func newEngine(cfg Config, embedding EmbeddingConfig) (*hugr.Service, error) {
	poolSettings := db.Settings{
		Timezone:      cfg.DB.Settings.Timezone,
		HomeDirectory: cfg.DB.Settings.HomeDirectory,
		MaxMemory:     cfg.DB.Settings.MaxMemory,
		WorkerThreads: cfg.DB.Settings.WorkerThreads,
	}
	service, err := hugr.New(hugr.Config{
		DB:   db.Config{Settings: poolSettings},
		Auth: &auth.Config{},
		CoreDB: coredb.New(coredb.Config{
			Path:       cfg.DB.Path,
			VectorSize: embedding.Dimension,
		}),
	})
	if err != nil {
		return nil, fmt.Errorf("engine construct: %w", err)
	}
	return service, nil
}

// attachHubDB wires hub.db into the engine as the "hub.db" RuntimeSource.
func attachHubDB(ctx context.Context, service *hugr.Service, cfg Config, embedding EmbeddingConfig) error {
	source := NewSource(SourceConfig{
		Path:          cfg.MemoryPath,
		VectorSize:    embedding.Dimension,
		EmbedderModel: embedding.Model,
	})
	if err := service.AttachRuntimeSource(ctx, source); err != nil {
		return fmt.Errorf("attach hub.db: %w", err)
	}
	return nil
}

// registerModelSources registers every cfg.Models entry in the engine
// as a data source. ${ENV_VAR} references in Path are expanded.
//
// core.data_sources rows persist in engine.db across restarts, so a
// plain insert would hit a PK violation on every restart. config.yaml
// is the source of truth for paths (API keys, model names, timeouts);
// we bulk-delete the existing rows first so edits propagate on every
// startup. Per-row insert failures warn and continue.
func registerModelSources(ctx context.Context, engine *hugr.Service, models []ModelDef, logger *slog.Logger) {
	if len(models) == 0 {
		return
	}
	names := make([]string, 0, len(models))
	for _, m := range models {
		names = append(names, m.Name)
	}
	if err := deleteDataSources(ctx, engine, names); err != nil {
		logger.Warn("data source bulk delete failed — insert may hit PK conflict",
			"err", err)
	}

	for _, m := range models {
		ds := types.DataSource{
			Name:     m.Name,
			Type:     types.DataSourceType(m.Type),
			Prefix:   m.Name,
			AsModule: false,
			Path:     os.ExpandEnv(m.Path),
			Sources:  []types.CatalogSource{},
		}
		if err := engine.RegisterDataSource(ctx, ds); err != nil {
			logger.Warn("data source registration failed",
				"name", m.Name, "type", m.Type, "err", err)
			continue
		}
		logger.Info("data source registered", "name", m.Name, "type", m.Type)
	}
}

// deleteDataSources unloads every name (ignoring errors — a row that
// doesn't exist is fine) then drops all matching rows in one mutation.
func deleteDataSources(ctx context.Context, engine *hugr.Service, names []string) error {
	for _, n := range names {
		_ = engine.UnloadDataSource(ctx, n)
	}
	nAny := make([]any, len(names))
	for i, n := range names {
		nAny[i] = n
	}
	res, err := engine.Query(ctx,
		`mutation ($names: [String!]!) {
			core { delete_data_sources(filter: {name: {in: $names}}) { success message } }
		}`,
		map[string]any{"names": nAny},
	)
	if err != nil {
		return err
	}
	defer res.Close()
	return res.Err()
}

// verifyLocalEmbedding runs a probe against the local embedding model
// and fails when its dimension disagrees with embedding.Dimension.
// A transport-level probe failure only warns — FTS fallback covers
// the case. A dim mismatch is fatal because existing vectors in
// memory_items are not re-computable.
func verifyLocalEmbedding(ctx context.Context, service *hugr.Service, embedding EmbeddingConfig, logger *slog.Logger) error {
	if embedding.Mode != "local" || embedding.Model == "" {
		return nil
	}
	dim, err := probeEmbedding(ctx, service, embedding.Model)
	if err != nil {
		logger.Warn("embedding probe failed — FTS fallback", "err", err)
		return nil
	}
	if dim != embedding.Dimension {
		return fmt.Errorf(
			"embedding dimension mismatch: config=%d provider=%d (model=%s). "+
				"Update cfg.Embedding.Dimension or recreate the agent",
			embedding.Dimension, dim, embedding.Model)
	}
	logger.Info("embedding verified", "model", embedding.Model, "dimension", dim)
	return nil
}

// probeEmbedding issues a single core.models.embedding call and
// returns the observed vector length. Used to detect dimension drift
// before any memory_item is written.
//
// Vector comes back wire-encoded as a quoted string (`"[0.1, 0.2, …]"`)
// because types.Vector has a custom MarshalJSON; types.Vector's
// matching UnmarshalJSON decodes that string back into []float64.
func probeEmbedding(ctx context.Context, engine *hugr.Service, model string) (int, error) {
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
		Vector types.Vector `json:"vector"`
	}
	if err := resp.ScanData("function.core.models.embedding", &result); err != nil {
		return 0, fmt.Errorf("embedding scan: %w", err)
	}
	return result.Vector.Len(), nil
}
