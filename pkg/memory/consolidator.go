package memory

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/hugr-lab/hugen/pkg/store"
)

// Consolidator performs idempotent background maintenance: deletes
// expired memory items and expires stale hypotheses. Runs under the
// scheduler's priority-30 cron lane (ADR v7.2 §8).
type Consolidator struct {
	hub              store.DB
	hypothesisExpiry time.Duration
	logger           *slog.Logger
}

// ConsolidatorOptions bundles consolidator construction parameters.
type ConsolidatorOptions struct {
	Hub              store.DB
	HypothesisExpiry time.Duration
	Logger           *slog.Logger
}

// NewConsolidator builds a Consolidator. HypothesisExpiry defaults to
// 30 days when zero.
func NewConsolidator(opts ConsolidatorOptions) (*Consolidator, error) {
	if opts.Hub == nil {
		return nil, fmt.Errorf("learning: Consolidator requires Hub")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.HypothesisExpiry <= 0 {
		opts.HypothesisExpiry = 30 * 24 * time.Hour
	}
	return &Consolidator{
		hub:              opts.Hub,
		hypothesisExpiry: opts.HypothesisExpiry,
		logger:           opts.Logger,
	}, nil
}

// Run is the consolidation pass. Order: delete expired facts, then
// expire old hypotheses. Both operations are idempotent — re-running
// immediately after a previous pass is safe (affects 0 rows).
func (c *Consolidator) Run(ctx context.Context) error {
	factsDeleted, err := c.hub.DeleteExpired(ctx)
	if err != nil {
		c.logger.Warn("consolidator: DeleteExpired", "err", err)
	}
	hypsExpired, err := c.hub.ExpireOldHypotheses(ctx, c.hypothesisExpiry)
	if err != nil {
		c.logger.Warn("consolidator: ExpireOldHypotheses", "err", err)
	}
	c.logger.Info("consolidator: run complete",
		"facts_deleted", factsDeleted,
		"hypotheses_expired", hypsExpired,
		"hypothesis_expiry", c.hypothesisExpiry)
	return nil
}
