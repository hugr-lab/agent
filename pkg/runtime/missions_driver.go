package runtime

import (
	"context"
	"log/slog"

	hugen "github.com/hugr-lab/hugen/pkg/agent"
	"github.com/hugr-lab/hugen/pkg/missions/executor"
	"github.com/hugr-lab/hugen/pkg/missions/graph"
	"github.com/hugr-lab/hugen/pkg/skills"
)

// dispatcherMissionDriver adapts pkg/agent.Dispatcher to the
// executor.MissionDriver interface. Resolves the role's
// SubAgentSpec from the skills catalog at RunMission time — keeps
// the Executor decoupled from skills.Manager — then calls
// Dispatcher.RunMission with the pre-assigned child session id and
// mission-graph metadata.
type dispatcherMissionDriver struct {
	dispatcher *hugen.Dispatcher
	skills     skills.Manager
	logger     *slog.Logger
}

var _ executor.MissionDriver = (*dispatcherMissionDriver)(nil)

func (d *dispatcherMissionDriver) RunMission(
	ctx context.Context,
	args graph.DispatchArgs,
) graph.DispatchResult {
	sk, err := d.skills.Load(ctx, args.Skill)
	if err != nil {
		return graph.DispatchResult{
			Status: graph.StatusFailed,
			Error:  "missions: load skill " + args.Skill + ": " + err.Error(),
		}
	}
	spec, ok := sk.SubAgents[args.Role]
	if !ok {
		return graph.DispatchResult{
			Status: graph.StatusFailed,
			Error:  "missions: unknown role " + args.Skill + "/" + args.Role,
		}
	}
	res, runErr := d.dispatcher.RunMission(ctx,
		args.ParentSessionID, "",
		args.Skill, args.Role,
		spec,
		args.Task, args.Notes,
		hugen.DispatchOverrides{
			ChildSessionID: args.ChildSessionID,
			CoordSessionID: args.CoordSessionID,
			DependsOn:      args.DependsOn,
		},
	)
	if runErr != nil {
		return graph.DispatchResult{
			Status: graph.StatusFailed,
			Error:  "missions: dispatch: " + runErr.Error(),
		}
	}
	status := graph.StatusDone
	if res.Error != "" {
		switch {
		case contains(res.Error, "turn cap"), contains(res.Error, "cancelled"):
			status = graph.StatusAbandoned
		default:
			status = graph.StatusFailed
		}
	}
	return graph.DispatchResult{
		Status:    status,
		Summary:   res.Summary,
		TurnsUsed: res.TurnsUsed,
		Error:     res.Error,
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
