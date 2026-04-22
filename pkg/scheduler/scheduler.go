package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	learnstore "github.com/hugr-lab/hugen/pkg/memory/learning/store"
	memstore "github.com/hugr-lab/hugen/pkg/memory/store"
	"github.com/hugr-lab/query-engine/types"
)

// Reviewer, Verifier, and Consolidator are the contracts the scheduler
// drives. All three live in pkg/learning; they're copied here as tiny
// interfaces so pkg/scheduler does not depend on pkg/learning (and
// hence does not pull the LLM deps into the scheduler's compilation
// unit).
type (
	Reviewer interface {
		Review(ctx context.Context, sessionID string) error
	}
	Verifier interface {
		VerifyNext(ctx context.Context) error
	}
	Consolidator interface {
		Run(ctx context.Context) error
	}

	// LearningStore is the narrow slice of the learning client the
	// scheduler needs. Declared locally so tests can substitute stubs
	// without building a full *learnstore.Client.
	LearningStore interface {
		ListPendingReviews(ctx context.Context, limit int) ([]learnstore.Review, error)
	}
)

// Runtime is the scheduler's construction surface. The scheduler
// builds its own memstore + learnstore clients internally from
// Querier + AgentID + AgentShort. Callers may still inject a custom
// LearningStore (for tests) — when set, it takes precedence over the
// one constructed from Querier.
type Runtime struct {
	// Interval is the baseline tick at which the scheduler polls for
	// work. Default: 30s.
	Interval time.Duration

	// ReviewDelay is the lag between a session closing (QueueReview)
	// and the first attempt to run its review — gives the async
	// classifier time to flush the transcript. Default: 2s.
	ReviewDelay time.Duration

	// ConsolidationAt is a 5-field cron expression describing when to
	// run the daily consolidation pass. Empty = never run
	// consolidation. Default: "0 3 * * *".
	ConsolidationAt string

	Reviewer     Reviewer
	Verifier     Verifier
	Consolidator Consolidator

	// Querier + AgentID + AgentShort are used to build the scheduler's
	// narrow LearningStore view internally. Required unless Learning is
	// explicitly injected.
	Querier    types.Querier
	AgentID    string
	AgentShort string

	// Learning is an optional pre-built narrow LearningStore. When
	// non-nil, it overrides the one that would otherwise be built from
	// Querier. Intended for tests with stubbed backends.
	Learning LearningStore

	Logger *slog.Logger
}

// Scheduler picks the highest-priority pending work on every tick.
type Scheduler struct {
	cfg      Runtime
	memory   *memstore.Client
	learning LearningStore
	wake     chan struct{}
	done     chan struct{}
	mu       sync.Mutex
	queue    []string // pending review session IDs awaiting nudge
}

// New constructs a Scheduler. Fills in defaults but does not start
// any goroutine. Start(ctx) kicks the loop off.
func New(cfg Runtime) (*Scheduler, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.ReviewDelay <= 0 {
		cfg.ReviewDelay = 2 * time.Second
	}
	if cfg.ConsolidationAt == "" {
		cfg.ConsolidationAt = "0 3 * * *"
	}
	// Validate cron expression upfront; we don't actually evaluate it
	// in the skeleton, but fail fast on syntax errors.
	if _, err := Parse(cfg.ConsolidationAt); err != nil {
		return nil, err
	}

	var memC *memstore.Client
	learning := cfg.Learning
	if cfg.Querier != nil {
		m, err := memstore.New(cfg.Querier, memstore.Options{
			AgentID: cfg.AgentID, AgentShort: cfg.AgentShort, Logger: cfg.Logger,
		})
		if err != nil {
			return nil, fmt.Errorf("scheduler: build memory store: %w", err)
		}
		memC = m
		if learning == nil {
			l, err := learnstore.New(cfg.Querier, learnstore.Options{
				AgentID: cfg.AgentID, AgentShort: cfg.AgentShort, Logger: cfg.Logger,
			})
			if err != nil {
				return nil, fmt.Errorf("scheduler: build learning store: %w", err)
			}
			learning = l
		}
	}
	if learning == nil {
		return nil, fmt.Errorf("scheduler: Querier or Learning required")
	}

	return &Scheduler{
		cfg:      cfg,
		memory:   memC,
		learning: learning,
		wake:     make(chan struct{}, 1),
		done:     make(chan struct{}),
	}, nil
}

// Start runs the scheduler loop until ctx is cancelled. Returns
// immediately; the loop runs in its own goroutine. Call Done() to
// wait for the loop to finish after ctx is cancelled.
//
// The skeleton loop ticks on cfg.Interval and wakes on QueueReview
// nudges but does not yet pick any work — concrete task pickers land
// in Phase 3 T050 (review), Phase 9 T090 (hypothesis + consolidation).
func (s *Scheduler) Start(ctx context.Context) {
	go s.run(ctx)
}

// Done returns a channel closed after the scheduler loop exits.
func (s *Scheduler) Done() <-chan struct{} { return s.done }

// Stop cancels the loop's parent context (if the caller holds it) and
// waits for the loop to exit. In practice main.go cancels the parent
// context directly and then reads Done(); this helper is kept for
// tests that want a synchronous wait with a timeout.
func (s *Scheduler) Stop(ctx context.Context) error {
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// QueueReview marks a session for post-session review. Idempotent:
// multiple queues for the same session produce a single pending
// session_reviews row (written by the reviewer) and a single wake-up.
func (s *Scheduler) QueueReview(sessionID string) {
	if sessionID == "" {
		return
	}
	s.mu.Lock()
	s.queue = append(s.queue, sessionID)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Scheduler) run(ctx context.Context) {
	defer close(s.done)
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	s.cfg.Logger.Info("scheduler started",
		"interval", s.cfg.Interval,
		"review_delay", s.cfg.ReviewDelay,
		"consolidation_at", s.cfg.ConsolidationAt)
	for {
		select {
		case <-ctx.Done():
			s.cfg.Logger.Info("scheduler stopping")
			return
		case <-t.C:
			s.tick(ctx)
		case <-s.wake:
			// Wait ReviewDelay so the classifier has time to flush
			// the closed session's transcript before the reviewer
			// reads it. Honour ctx cancel during the delay.
			select {
			case <-ctx.Done():
				return
			case <-time.After(s.cfg.ReviewDelay):
			}
			s.tick(ctx)
		}
	}
}

// tick runs one priority-ordered pass over pending work. Priority 10
// (reviews) first, then 20 (hypotheses), then 30 (consolidation) —
// matching ADR v7.2 §8. Each lane processes at most one unit of work
// per tick so user-facing turns never stall behind a long backlog.
func (s *Scheduler) tick(ctx context.Context) {
	if s.pickReview(ctx) {
		return
	}
	if s.pickHypothesis(ctx) {
		return
	}
	s.maybeConsolidate(ctx)
}

// pickReview runs the oldest pending review in one tick. Returns true
// if work was picked (so other lanes sit out the tick).
func (s *Scheduler) pickReview(ctx context.Context) bool {
	if s.cfg.Reviewer == nil || s.learning == nil {
		return false
	}
	pending, err := s.learning.ListPendingReviews(ctx, 1)
	if err != nil {
		s.cfg.Logger.Warn("scheduler: ListPendingReviews", "err", err)
		return false
	}
	if len(pending) == 0 {
		return false
	}
	sessionID := pending[0].SessionID
	if err := s.cfg.Reviewer.Review(ctx, sessionID); err != nil {
		s.cfg.Logger.Warn("scheduler: review failed", "session", sessionID, "err", err)
	}
	return true
}

// pickHypothesis runs one hypothesis verification. Priority order
// high → medium → low until we find work. Placeholder wiring: the
// Verifier lands in Phase 9 (US6); until then cfg.Verifier is nil
// and this returns false cleanly.
func (s *Scheduler) pickHypothesis(ctx context.Context) bool {
	if s.cfg.Verifier == nil {
		return false
	}
	if err := s.cfg.Verifier.VerifyNext(ctx); err != nil {
		s.cfg.Logger.Warn("scheduler: verify failed", "err", err)
	}
	return true
}

// maybeConsolidate fires the consolidator when the cron schedule
// matches. Evaluated per-tick; simple minute-granularity check.
func (s *Scheduler) maybeConsolidate(ctx context.Context) {
	if s.cfg.Consolidator == nil {
		return
	}
	now := time.Now()
	sched, err := Parse(s.cfg.ConsolidationAt)
	if err != nil {
		return
	}
	// Only trigger on the exact minute the schedule fires.
	if !sched.matches(now) {
		return
	}
	if err := s.cfg.Consolidator.Run(ctx); err != nil {
		s.cfg.Logger.Warn("scheduler: consolidate failed", "err", err)
	}
}
