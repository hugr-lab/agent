package executor

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/missions/graph"
)

// MissionDriver is the Executor's boundary with the actual sub-agent
// dispatcher. Production wiring passes a pkg/agent.Dispatcher adapter;
// tests pass a scripted fake that returns canned DispatchResults.
//
// Implementations MUST honour the pre-assigned ChildSessionID and
// write the mission session row themselves (production does this via
// sessions.Manager.Create with the mission metadata; the test fake
// emulates the same shape).
type MissionDriver interface {
	RunMission(ctx context.Context, args graph.DispatchArgs) graph.DispatchResult
}
