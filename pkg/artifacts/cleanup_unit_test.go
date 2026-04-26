package artifacts

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	artstore "github.com/hugr-lab/hugen/pkg/artifacts/store"
)

func nopLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestEligibleForCleanup is the pure-Go regression test for the
// `break` bug that previously caused fetchCleanupCandidates to
// only return TTLSession rows. Drives eligibleForCleanup directly
// with synthesized records — no engine, no timezone risk.
func TestEligibleForCleanup(t *testing.T) {
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	week := func(d time.Duration) artstore.Record {
		return artstore.Record{ID: "art_w", TTL: "7d", CreatedAt: now.Add(-d)}
	}
	month := func(d time.Duration) artstore.Record {
		return artstore.Record{ID: "art_m", TTL: "30d", CreatedAt: now.Add(-d)}
	}
	perm := artstore.Record{ID: "art_p", TTL: "permanent", CreatedAt: now.Add(-30 * 24 * time.Hour)}

	cases := []struct {
		name string
		rec  artstore.Record
		want bool
	}{
		// 7d default threshold (604800s).
		{"7d fresh", week(time.Hour), false},
		{"7d at boundary", week(7 * 24 * time.Hour), false}, // strict Before
		{"7d past boundary", week(8 * 24 * time.Hour), true},
		// 30d default threshold (2592000s).
		{"30d fresh", month(time.Hour), false},
		{"30d past boundary", month(31 * 24 * time.Hour), true},
		// permanent never expires.
		{"permanent old", perm, false},
		// unknown class falls through to false.
		{"unknown ttl", artstore.Record{ID: "x", TTL: "lifetime"}, false},
	}

	m := &Manager{cfg: Config{}, log: nopLogger()}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := m.eligibleForCleanup(context.Background(), c.rec, now)
			assert.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}
}

// TestEligibleForCleanup_OperatorOverrides verifies the operator's
// cfg.TTL7dSeconds / cfg.TTL30dSeconds knobs override the defaults.
// Same code path as the production cron — guards against the
// `break` bug regressing in either branch.
func TestEligibleForCleanup_OperatorOverrides(t *testing.T) {
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)

	// Tight 1-hour windows on both classes.
	m := &Manager{
		cfg: Config{TTL7dSeconds: 3600, TTL30dSeconds: 3600},
		log: nopLogger(),
	}

	for _, ttl := range []string{"7d", "30d"} {
		fresh := artstore.Record{ID: "f_" + ttl, TTL: ttl, CreatedAt: now.Add(-30 * time.Minute)}
		stale := artstore.Record{ID: "s_" + ttl, TTL: ttl, CreatedAt: now.Add(-2 * time.Hour)}

		got, err := m.eligibleForCleanup(context.Background(), fresh, now)
		assert.NoError(t, err)
		assert.False(t, got, "%s within window must NOT be eligible", ttl)

		got, err = m.eligibleForCleanup(context.Background(), stale, now)
		assert.NoError(t, err)
		assert.True(t, got, "%s past 1h override must be eligible", ttl)
	}
}
