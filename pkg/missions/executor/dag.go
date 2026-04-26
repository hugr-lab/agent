package executor

import (
	"context"
	"sync"
	"time"
)

// dag is one coordinator session's mission graph. Lives in
// Executor.dags keyed by coord session id; its own mu guards the
// per-coordinator mission map so Tick can reconcile multiple
// coordinators concurrently (the top-level tickMu just gates
// overlapping Tick calls).
type dag struct {
	coordID         string
	mu              sync.Mutex
	missions        map[string]*missionNode
	completionFired bool
}

// missionNode is the in-memory per-mission state. Not persisted —
// Executor.RestoreState rebuilds one from persisted rows at boot.
type missionNode struct {
	id             string
	coordID        string
	skill          string
	role           string
	task           string
	status         string
	upstream       []string
	downstream     []string
	inputArtifacts []string
	resultCh       chan missionResult
	cancel         context.CancelFunc
	startedAt      time.Time
	terminated     time.Time
	turnsUsed      int
	summary        string
	reason         string
	spawnEventID   string
}

// missionResult is what a dispatcher goroutine writes to its mission's
// terminal channel. Executor.drainTerminals reads it on the next Tick.
type missionResult struct {
	status       string
	summary      string
	turnsUsed    int
	durationMs   int64
	abstained    bool
	abstainedWhy string
	errorMsg     string
}
