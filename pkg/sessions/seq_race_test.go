//go:build duckdb_arrow

package sessions

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	adksession "google.golang.org/adk/session"

	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
)

// TestSeqRace_SessionAppendEvent fires a burst of concurrent
// transcript writes against the same session via Session.AppendEvent
// (synchronous path) and Manager.PublishHubEvent (the async path the
// classifier uses). Every row MUST land with a distinct seq value —
// that's the whole point of the per-session writeMu.
//
// Without the lock the live agent regularly produces seq collisions
// between Session.LoadSkill (sync) and the classifier's tool_call
// inserts (async).
func TestSeqRace_SessionAppendEvent(t *testing.T) {
	m, _ := newManagerWithHub(t)
	ctx := context.Background()

	_, err := m.Create(ctx, &adksession.CreateRequest{
		AppName: "hugr_agent", UserID: "u-1", SessionID: "sess-seq-race",
	})
	require.NoError(t, err)
	sess, err := m.Session("sess-seq-race")
	require.NoError(t, err)

	const writers = 30
	var wg sync.WaitGroup
	wg.Add(writers)
	errCh := make(chan error, writers)

	for i := 0; i < writers; i++ {
		go func(i int) {
			defer wg.Done()
			ev := sessstore.Event{
				EventType: sessstore.EventTypeUserMessage,
				Author:    "user",
				Content:   "msg",
			}
			// Half through Session.AppendEvent (the sync path),
			// half through Manager.PublishHubEvent (the classifier
			// path) — both must funnel through the same writeMu.
			if i%2 == 0 {
				_, err := sess.AppendEvent(ctx, ev, "")
				errCh <- err
				return
			}
			_, err := m.PublishHubEvent(ctx, "sess-seq-race", ev, "")
			errCh <- err
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}

	rows, err := m.hub.GetEvents(ctx, "sess-seq-race")
	require.NoError(t, err)
	// `writers` user_message rows from the test + any autoload
	// skill_loaded events the manager wrote on Create. The invariant
	// the lock guarantees is that every row carries a UNIQUE seq —
	// total count includes both kinds.
	require.GreaterOrEqual(t, len(rows), writers, "every concurrent write should land")

	userRows := 0
	seen := map[int]bool{}
	for _, ev := range rows {
		assert.False(t, seen[ev.Seq], "seq %d collided — per-session lock is broken", ev.Seq)
		seen[ev.Seq] = true
		if ev.EventType == sessstore.EventTypeUserMessage {
			userRows++
		}
	}
	assert.Equal(t, writers, userRows,
		"all %d concurrent user_message writes must persist", writers)
	assert.Equal(t, len(rows), len(seen), "every seq must be distinct")
}
