package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, nil))
}

func TestParse_Valid(t *testing.T) {
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

func TestScheduler_EveryFires(t *testing.T) {
	s := New(quietLogger())
	var calls atomic.Int64
	require.NoError(t, s.Every("t", 10*time.Millisecond, func(ctx context.Context) error {
		calls.Add(1)
		return nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	// Within 80ms we expect at least 3 invocations (10ms interval).
	require.Eventually(t, func() bool { return calls.Load() >= 3 }, 200*time.Millisecond, 5*time.Millisecond)
}

func TestScheduler_WakeFiresImmediately(t *testing.T) {
	s := New(quietLogger())
	var calls atomic.Int64
	require.NoError(t, s.Every("t", time.Hour, func(ctx context.Context) error {
		calls.Add(1)
		return nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	s.Wake("t")
	require.Eventually(t, func() bool { return calls.Load() >= 1 }, 200*time.Millisecond, 5*time.Millisecond)
}

func TestScheduler_WakeUnknownNoop(t *testing.T) {
	s := New(quietLogger())
	// Should not panic when waking a task that was never registered.
	s.Wake("nobody-home")
}

func TestScheduler_ErrorLoggedAndLoopContinues(t *testing.T) {
	s := New(quietLogger())
	var calls atomic.Int64
	require.NoError(t, s.Every("t", 10*time.Millisecond, func(ctx context.Context) error {
		calls.Add(1)
		return errors.New("boom")
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	// Two consecutive invocations prove the loop did not exit after the first error.
	require.Eventually(t, func() bool { return calls.Load() >= 2 }, 200*time.Millisecond, 5*time.Millisecond)
}

func TestScheduler_DoneAfterCtxCancel(t *testing.T) {
	s := New(quietLogger())
	require.NoError(t, s.Every("t", time.Hour, func(ctx context.Context) error { return nil }))
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)
	cancel()
	select {
	case <-s.Done():
	case <-time.After(time.Second):
		t.Fatal("scheduler did not exit on ctx cancel")
	}
}

func TestScheduler_DuplicateRegistration(t *testing.T) {
	s := New(quietLogger())
	task := func(ctx context.Context) error { return nil }
	require.NoError(t, s.Every("dup", time.Second, task))
	err := s.Every("dup", time.Second, task)
	assert.Error(t, err)
}

func TestScheduler_RegisterAfterStart(t *testing.T) {
	s := New(quietLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	err := s.Every("late", time.Second, func(ctx context.Context) error { return nil })
	assert.Error(t, err)
}

func TestScheduler_CronRegistration(t *testing.T) {
	s := New(quietLogger())
	err := s.Cron("daily", "0 3 * * *", func(ctx context.Context) error { return nil })
	require.NoError(t, err)

	err = s.Cron("bad", "not-a-cron", func(ctx context.Context) error { return nil })
	assert.Error(t, err)
}
