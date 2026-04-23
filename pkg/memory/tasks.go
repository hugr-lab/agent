package memory

import (
	"fmt"
	"time"

	"github.com/hugr-lab/hugen/pkg/scheduler"
)

// Default task cadence used when SchedulerConfig leaves a field at
// zero. Matches the old scheduler defaults so existing deployments
// behave identically after the refactor.
const (
	defaultInterval    = 30 * time.Second
	defaultReviewDelay = 2 * time.Second
	defaultCron        = "0 3 * * *"
)

// Register wires the three memory tasks into sched:
//
//   - "memory.review"       — Every(cfg.Interval), Reviewer.Tick.
//   - "memory.verify"       — Every(cfg.Interval), Verifier.VerifyNext.
//   - "memory.consolidate"  — Cron(cfg.ConsolidationAt), Consolidator.Run.
//
// The reviewer is also bound to sched (via bindScheduler) so its
// QueueReview method can Wake("memory.review") after ReviewDelay —
// sessions.Manager continues calling QueueReview on Delete through
// the narrow ReviewQueuer interface, but the implementation now
// lives in pkg/memory instead of pkg/scheduler.
//
// Empty ConsolidationAt skips consolidator registration.
func Register(
	sched *scheduler.Scheduler,
	reviewer *Reviewer,
	verifier *Verifier,
	consolidator *Consolidator,
	cfg SchedulerConfig,
) error {
	if sched == nil {
		return fmt.Errorf("memory.Register: nil scheduler")
	}
	if reviewer == nil {
		return fmt.Errorf("memory.Register: nil reviewer")
	}

	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	delay := cfg.ReviewDelay
	if delay <= 0 {
		delay = defaultReviewDelay
	}
	cron := cfg.ConsolidationAt
	if cron == "" {
		cron = defaultCron
	}

	reviewer.bindScheduler(sched, delay)
	if err := sched.Every(reviewTaskName, interval, reviewer.Tick); err != nil {
		return fmt.Errorf("memory.Register: register review task: %w", err)
	}
	if verifier != nil {
		if err := sched.Every("memory.verify", interval, verifier.VerifyNext); err != nil {
			return fmt.Errorf("memory.Register: register verify task: %w", err)
		}
	}
	if consolidator != nil && cron != "" {
		if err := sched.Cron("memory.consolidate", cron, consolidator.Run); err != nil {
			return fmt.Errorf("memory.Register: register consolidate task: %w", err)
		}
	}
	return nil
}
