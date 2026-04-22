// Package scheduler is a small periodic task runner. Each registered
// task owns its own goroutine; tasks are opaque closures from the
// scheduler's perspective — no domain knowledge lives here.
//
// Registration:
//
//	sched := scheduler.New(logger)
//	sched.Every("foo", 30*time.Second, fooTask)
//	sched.Cron("bar", "0 3 * * *", barTask)
//	sched.Start(ctx)
//
// Wake(name) nudges a task so its next run fires immediately instead
// of waiting for the interval or cron match. Useful for external
// signals (e.g., memory's post-session review queue).
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Task is one unit of periodic work. Return errors are logged and do
// not stop future invocations.
type Task func(ctx context.Context) error

// Scheduler runs registered tasks. Each task lives in its own
// goroutine; Wake signals that goroutine without touching other tasks.
type Scheduler struct {
	logger *slog.Logger

	mu      sync.Mutex
	entries map[string]*entry

	started bool
	wg      sync.WaitGroup
	done    chan struct{}
}

type entry struct {
	name     string
	task     Task
	interval time.Duration // 0 when cron-scheduled
	cron     *Schedule     // nil when interval-scheduled
	wake     chan struct{} // buffered size 1
}

// New constructs an empty Scheduler. Logger defaults to slog.Default.
func New(logger *slog.Logger) *Scheduler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{
		logger:  logger,
		entries: make(map[string]*entry),
		done:    make(chan struct{}),
	}
}

// Every registers task to run every interval. Must be called before
// Start. Returns an error on duplicate name, zero/negative interval,
// or after Start.
func (s *Scheduler) Every(name string, interval time.Duration, task Task) error {
	if interval <= 0 {
		return fmt.Errorf("scheduler: Every %q: interval must be positive, got %s", name, interval)
	}
	return s.register(name, interval, nil, task)
}

// Cron registers task on a 5-field cron expression. Must be called
// before Start. Returns an error on duplicate name, invalid spec, or
// after Start.
func (s *Scheduler) Cron(name string, spec string, task Task) error {
	c, err := Parse(spec)
	if err != nil {
		return fmt.Errorf("scheduler: Cron %q: %w", name, err)
	}
	return s.register(name, 0, &c, task)
}

func (s *Scheduler) register(name string, interval time.Duration, cron *Schedule, task Task) error {
	if name == "" {
		return fmt.Errorf("scheduler: register: empty name")
	}
	if task == nil {
		return fmt.Errorf("scheduler: register %q: nil task", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return fmt.Errorf("scheduler: register %q: already started", name)
	}
	if _, exists := s.entries[name]; exists {
		return fmt.Errorf("scheduler: register %q: duplicate", name)
	}
	s.entries[name] = &entry{
		name:     name,
		task:     task,
		interval: interval,
		cron:     cron,
		wake:     make(chan struct{}, 1),
	}
	return nil
}

// Wake nudges the named task to run on the next loop iteration,
// bypassing its normal wait. Non-blocking; a pending wake that hasn't
// been consumed swallows additional Wake calls — the task sees at
// most one extra run per wake-channel slot.
func (s *Scheduler) Wake(name string) {
	s.mu.Lock()
	e, ok := s.entries[name]
	s.mu.Unlock()
	if !ok {
		return
	}
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Start launches one goroutine per registered task. Returns
// immediately. Call Done() to await shutdown after ctx is cancelled.
// Idempotent — repeated calls after the first are no-ops.
func (s *Scheduler) Start(ctx context.Context) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	entries := make([]*entry, 0, len(s.entries))
	for _, e := range s.entries {
		entries = append(entries, e)
	}
	s.mu.Unlock()

	s.logger.Info("scheduler started", "tasks", len(entries))
	for _, e := range entries {
		s.wg.Add(1)
		go s.runTask(ctx, e)
	}
	go func() {
		s.wg.Wait()
		close(s.done)
		s.logger.Info("scheduler stopped")
	}()
}

// Done returns a channel closed after every task goroutine has exited.
func (s *Scheduler) Done() <-chan struct{} { return s.done }

func (s *Scheduler) runTask(ctx context.Context, e *entry) {
	defer s.wg.Done()
	for {
		wait := s.waitDuration(e)
		var timer <-chan time.Time
		if wait > 0 {
			t := time.NewTimer(wait)
			timer = t.C
			defer func() {
				if !t.Stop() {
					select {
					case <-t.C:
					default:
					}
				}
			}()
		}
		select {
		case <-ctx.Done():
			return
		case <-timer:
		case <-e.wake:
		}
		if ctx.Err() != nil {
			return
		}
		if err := e.task(ctx); err != nil {
			s.logger.Warn("scheduler: task failed", "name", e.name, "err", err)
		}
	}
}

// waitDuration returns how long the runTask loop should sleep before
// the next invocation. For interval tasks it's the full interval; for
// cron tasks it's time until the next minute match.
func (s *Scheduler) waitDuration(e *entry) time.Duration {
	if e.cron != nil {
		next := nextCronMatch(e.cron, time.Now())
		d := time.Until(next)
		if d < 0 {
			return 0
		}
		return d
	}
	return e.interval
}

// nextCronMatch walks forward from `from` one minute at a time until
// the schedule matches. Minute granularity — adequate for the only
// cron we care about (daily consolidation). Caps search at 25h so a
// degenerate schedule never loops forever.
func nextCronMatch(c *Schedule, from time.Time) time.Time {
	t := from.Truncate(time.Minute).Add(time.Minute)
	for range 25 * 60 {
		if c.matches(t) {
			return t
		}
		t = t.Add(time.Minute)
	}
	return from.Add(24 * time.Hour) // fallback; should not hit
}
