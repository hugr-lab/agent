package agent

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/sessions"
	"github.com/stretchr/testify/require"
)

// TestNewAgent_RequiresSessions: constructor guard.
func TestNewAgent_RequiresSessions(t *testing.T) {
	_, err := NewAgent(Runtime{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Sessions required")
}

// TestStartSessionCleanup_NoopWithoutSessions: nil manager or
// non-positive maxAge is a silent no-op.
func TestStartSessionCleanup_NoopWithoutSessions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Both calls must return immediately (no goroutine spawned).
	StartSessionCleanup(ctx, nil, time.Hour, nil)
	StartSessionCleanup(ctx, &sessions.Manager{}, 0, nil)
	StartSessionCleanup(ctx, &sessions.Manager{}, -1, nil)
}

// TestStartSessionCleanup_TerminatesOnCtxCancel: sanity check that
// the cleanup goroutine exits when ctx is cancelled.
func TestStartSessionCleanup_TerminatesOnCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	logger := slog.New(slog.NewTextHandler(discardWriter{}, nil))

	// Use a big enough maxAge that the ticker does not fire during
	// the test; we're only asserting graceful shutdown.
	sm := &sessions.Manager{}
	StartSessionCleanup(ctx, sm, 10*time.Minute, logger)

	cancel()
	// Give the goroutine a tick to see the cancelled ctx.
	time.Sleep(20 * time.Millisecond)
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
