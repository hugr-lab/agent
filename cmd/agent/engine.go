package main

import (
	"context"
	"fmt"
	"log/slog"

	hugr "github.com/hugr-lab/query-engine"
	"github.com/hugr-lab/query-engine/pkg/auth"
	coredb "github.com/hugr-lab/query-engine/pkg/data-sources/sources/runtime/core-db"
	"github.com/hugr-lab/query-engine/pkg/db"

	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/store"
	"github.com/hugr-lab/hugen/pkg/store/migrate"
)

// buildLocalEngine provisions the memory DB, constructs an embedded hugr
// engine with CoreDB at HugrLocal.DB.Path, attaches hub.db as a runtime
// source, and initialises. Caller owns Close() (flushes DuckDB WAL).
func buildLocalEngine(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*hugr.Service, error) {
	seed := &migrate.SeedData{
		AgentType: migrate.SeedAgentType{
			ID:   cfg.Identity.Type,
			Name: cfg.Identity.Type,
		},
		Agent: migrate.SeedAgent{
			ID:      cfg.Identity.ID,
			ShortID: cfg.Identity.ShortID,
			Name:    cfg.Identity.Name,
		},
	}
	if err := migrate.Ensure(migrate.Config{
		Path:          cfg.Memory.Path,
		VectorSize:    cfg.Embedding.Dimension,
		EmbedderModel: cfg.Embedding.Model,
		Seed:          seed,
	}); err != nil {
		return nil, fmt.Errorf("migrate hub.db: %w", err)
	}
	logger.Info("hub.db provisioned",
		"path", cfg.Memory.Path,
		"version", migrate.SchemaVersion,
	)

	// In-memory working pool; persistent state lives in CoreDB (engine.db)
	// and the attached hub.db (memory.db).
	poolSettings := db.Settings{
		Timezone:      cfg.HugrLocal.DB.Settings.Timezone,
		HomeDirectory: cfg.HugrLocal.DB.Settings.HomeDirectory,
		MaxMemory:     cfg.HugrLocal.DB.Settings.MaxMemory,
		WorkerThreads: cfg.HugrLocal.DB.Settings.WorkerThreads,
	}

	service, err := hugr.New(hugr.Config{
		DB:   db.Config{Settings: poolSettings},
		Auth: &auth.Config{},
		CoreDB: coredb.New(coredb.Config{
			Path:       cfg.HugrLocal.DB.Path,
			VectorSize: cfg.Embedding.Dimension,
		}),
	})
	if err != nil {
		return nil, fmt.Errorf("models.NewHugr: %w", err)
	}

	source := store.NewSource(store.Config{
		Path:          cfg.Memory.Path,
		VectorSize:    cfg.Embedding.Dimension,
		EmbedderModel: cfg.Embedding.Model,
	})
	if err := service.AttachRuntimeSource(ctx, source); err != nil {
		_ = service.Close()
		return nil, fmt.Errorf("attach hub.db: %w", err)
	}

	if err := service.Init(ctx); err != nil {
		_ = service.Close()
		return nil, fmt.Errorf("engine.Init: %w", err)
	}

	logger.Info("local engine initialised",
		"core_db", cfg.HugrLocal.DB.Path,
		"hub_db", cfg.Memory.Path,
		"vector_size", cfg.Embedding.Dimension,
	)
	return service, nil
}
