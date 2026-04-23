package memory

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	learnstore "github.com/hugr-lab/hugen/pkg/memory/learning/store"
	memstore "github.com/hugr-lab/hugen/pkg/memory/store"
	"github.com/hugr-lab/query-engine/types"
)

// Consolidator performs idempotent background maintenance: deletes
// expired memory items and expires stale hypotheses. Runs under the
// scheduler's priority-30 cron lane (ADR v7.2 §8).
type Consolidator struct {
	memory           *memstore.Client
	learning         *learnstore.Client
	hypothesisExpiry time.Duration
	logger           *slog.Logger
}

// ConsolidatorOptions bundles consolidator construction parameters. The
// consolidator builds its own memstore + learnstore clients internally
// from Querier + AgentID + AgentShort.
type ConsolidatorOptions struct {
	Querier          types.Querier
	AgentID          string
	AgentShort       string
	HypothesisExpiry time.Duration
	Logger           *slog.Logger

	// Memory / Learning are optional pre-built clients. When set,
	// NewConsolidator skips its internal New() calls.
	Memory   *memstore.Client
	Learning *learnstore.Client
}

// NewConsolidator builds a Consolidator. HypothesisExpiry defaults to
// 30 days when zero.
func NewConsolidator(opts ConsolidatorOptions) (*Consolidator, error) {
	if opts.Querier == nil {
		return nil, fmt.Errorf("learning: Consolidator requires Querier")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.HypothesisExpiry <= 0 {
		opts.HypothesisExpiry = 30 * 24 * time.Hour
	}
	memC := opts.Memory
	if memC == nil {
		c, err := memstore.New(opts.Querier, memstore.Options{
			AgentID: opts.AgentID, AgentShort: opts.AgentShort, Logger: opts.Logger,
		})
		if err != nil {
			return nil, fmt.Errorf("learning: build memory store: %w", err)
		}
		memC = c
	}
	learnC := opts.Learning
	if learnC == nil {
		c, err := learnstore.New(opts.Querier, learnstore.Options{
			AgentID: opts.AgentID, AgentShort: opts.AgentShort, Logger: opts.Logger,
		})
		if err != nil {
			return nil, fmt.Errorf("learning: build learning store: %w", err)
		}
		learnC = c
	}
	return &Consolidator{
		memory:           memC,
		learning:         learnC,
		hypothesisExpiry: opts.HypothesisExpiry,
		logger:           opts.Logger,
	}, nil
}

// Run is the consolidation pass. Order: delete expired facts, then
// expire old hypotheses. Both operations are idempotent — re-running
// immediately after a previous pass is safe (affects 0 rows).
func (c *Consolidator) Run(ctx context.Context) error {
	factsDeleted, err := c.memory.DeleteExpired(ctx)
	if err != nil {
		c.logger.Warn("consolidator: DeleteExpired", "err", err)
	}
	hypsExpired, err := c.learning.ExpireOldHypotheses(ctx, c.hypothesisExpiry)
	if err != nil {
		c.logger.Warn("consolidator: ExpireOldHypotheses", "err", err)
	}
	c.logger.Info("consolidator: run complete",
		"facts_deleted", factsDeleted,
		"hypotheses_expired", hypsExpired,
		"hypothesis_expiry", c.hypothesisExpiry)
	return nil
}
