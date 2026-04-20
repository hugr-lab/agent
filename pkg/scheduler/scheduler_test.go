package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/interfaces"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse_Valid(t *testing.T) {
	// Our parser only supports * and single ints (constitution §V
	// own-impl slot); step / range / list syntax is out of scope.
	for _, c := range []string{"0 3 * * *", "15 14 1 * *", "* * * * *"} {
		_, err := Parse(c)
		require.NoError(t, err, c)
	}
}

func TestParse_Invalid(t *testing.T) {
	for _, c := range []string{"", "1 2 3", "bad cron", "70 * * * *"} {
		_, err := Parse(c)
		assert.Error(t, err, c)
	}
}

func TestSchedule_NextMatches(t *testing.T) {
	s, err := Parse("0 3 * * *")
	require.NoError(t, err)
	from := time.Date(2026, 4, 20, 2, 0, 0, 0, time.UTC)
	next := s.Next(from)
	assert.Equal(t, 3, next.Hour())
	assert.Equal(t, 0, next.Minute())
}

// stubHub lets us fake ListPendingReviews without a real engine.
type stubHub struct {
	interfaces.HubDB
	pending atomic.Value // []interfaces.SessionReview
}

func (s *stubHub) ListPendingReviews(ctx context.Context, limit int) ([]interfaces.SessionReview, error) {
	if v := s.pending.Load(); v != nil {
		return v.([]interfaces.SessionReview), nil
	}
	return nil, nil
}

type stubReviewer struct {
	calls atomic.Int64
	seen  chan string
	err   error
}

func newStubReviewer() *stubReviewer {
	return &stubReviewer{seen: make(chan string, 4)}
}

func (r *stubReviewer) Review(ctx context.Context, sessionID string) error {
	r.calls.Add(1)
	select {
	case r.seen <- sessionID:
	default:
	}
	return r.err
}

func TestScheduler_PicksPendingReview(t *testing.T) {
	hub := &stubHub{}
	hub.pending.Store([]interfaces.SessionReview{{ID: "rev1", SessionID: "sess1", Status: "pending"}})
	rv := newStubReviewer()

	s, err := New(Config{
		Interval:    20 * time.Millisecond,
		ReviewDelay: 5 * time.Millisecond,
		Reviewer:    rv,
		Hub:         hub,
		Logger:      slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)

	select {
	case got := <-rv.seen:
		assert.Equal(t, "sess1", got)
	case <-time.After(time.Second):
		t.Fatal("reviewer not invoked")
	}

	cancel()
	select {
	case <-s.Done():
	case <-time.After(time.Second):
		t.Fatal("scheduler did not exit on ctx cancel")
	}
}

func TestScheduler_QueueReviewWakes(t *testing.T) {
	hub := &stubHub{}
	rv := newStubReviewer()
	s, err := New(Config{
		Interval:    time.Hour, // so ticker doesn't fire
		ReviewDelay: 5 * time.Millisecond,
		Reviewer:    rv,
		Hub:         hub,
		Logger:      slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	// Populate pending list then nudge.
	hub.pending.Store([]interfaces.SessionReview{{ID: "r1", SessionID: "sX", Status: "pending"}})
	s.QueueReview("sX")

	select {
	case <-rv.seen:
	case <-time.After(time.Second):
		t.Fatal("reviewer not invoked")
	}
}

func TestScheduler_ReviewerErrorLogged(t *testing.T) {
	hub := &stubHub{}
	hub.pending.Store([]interfaces.SessionReview{{SessionID: "s1", Status: "pending"}})
	rv := newStubReviewer()
	rv.err = errors.New("boom")
	s, err := New(Config{
		Interval: 15 * time.Millisecond,
		Reviewer: rv, Hub: hub,
		Logger: slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	select {
	case <-rv.seen:
	case <-time.After(time.Second):
		t.Fatal("reviewer not invoked")
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestScheduler_StopTimeout — Stop(ctx) honours an explicit timeout,
// returning ctx.Err() when the loop has not exited yet. Covers the
// SC-004 invariant that shutdown is bounded.
func TestScheduler_StopTimeout(t *testing.T) {
	hub := &stubHub{}
	rv := newStubReviewer()
	s, err := New(Config{
		Interval: time.Hour, Reviewer: rv, Hub: hub,
		Logger: slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	// Don't cancel the loop — Stop with a short timeout returns DeadlineExceeded.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	err = s.Stop(stopCtx)
	stopCancel()
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	// Now cancel and wait properly.
	cancel()
	select {
	case <-s.Done():
	case <-time.After(time.Second):
		t.Fatal("scheduler did not exit after ctx cancel")
	}
}

// TestScheduler_IdempotentOnCrashResume — a review kept in status
// "pending" (e.g. because the process died mid-review) is picked up
// again on the next scheduler tick. We simulate this by leaving the
// stubHub's pending list populated and asserting the reviewer fires
// twice across two separate Scheduler lifetimes.
func TestScheduler_IdempotentOnCrashResume(t *testing.T) {
	hub := &stubHub{}
	hub.pending.Store([]interfaces.SessionReview{{ID: "r1", SessionID: "sess-crash", Status: "pending"}})

	// First run — picks the review, then "crashes" via ctx cancel.
	rv1 := newStubReviewer()
	s1, err := New(Config{
		Interval: 15 * time.Millisecond,
		Reviewer: rv1, Hub: hub,
		Logger: slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	})
	require.NoError(t, err)
	ctx1, cancel1 := context.WithCancel(context.Background())
	s1.Start(ctx1)
	select {
	case got := <-rv1.seen:
		assert.Equal(t, "sess-crash", got)
	case <-time.After(time.Second):
		t.Fatal("first scheduler did not invoke reviewer")
	}
	cancel1()
	<-s1.Done()

	// Second run — pending row is still there (hub not mutated because the
	// stub reviewer never called CompleteReview). Resumes cleanly.
	rv2 := newStubReviewer()
	s2, err := New(Config{
		Interval: 15 * time.Millisecond,
		Reviewer: rv2, Hub: hub,
		Logger: slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	})
	require.NoError(t, err)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	s2.Start(ctx2)
	select {
	case got := <-rv2.seen:
		assert.Equal(t, "sess-crash", got)
	case <-time.After(time.Second):
		t.Fatal("second scheduler did not resume pending review")
	}
}
