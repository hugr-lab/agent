// Package graph holds the value-types shared across the mission
// sub-domains (planner, executor, store, service). Kept minimal and
// dependency-free so everyone can import it without cycles.
//
// Concept map:
//   - Status values: runtime states of a mission.
//   - PlanResult + PlannerMission/Edge/Output: what the planner
//     produces and the executor consumes.
//   - DispatchArgs + DispatchResult: the boundary between the
//     executor and whatever mission driver (Dispatcher) runs a
//     single mission's turns.
//   - MissionRecord: blended runtime-view of a mission, consumed by
//     tools and by the executor's Snapshot.
//   - Metadata key constants: per-mission `sessions.metadata` JSON
//     keys. The in-memory executor DAG is authoritative during a
//     session's lifetime; metadata is the minimum needed to rebuild
//     the DAG after restart.
//   - Sentinel errors + ValidatePlan: enforce invariants up-front
//     (acyclic, no self-loops, known edges, non-empty tasks).
package graph

import (
	"errors"
	"fmt"
	"time"
)

// Status values for a mission's runtime state. Only StatusDone /
// StatusFailed / StatusAbandoned are persisted to sessions.status;
// pending / ready / running live in the Executor's in-memory DAG
// while the row stays `active` in hub.
const (
	StatusPending   = "pending"
	StatusReady     = "ready"
	StatusRunning   = "running"
	StatusDone      = "done"
	StatusFailed    = "failed"
	StatusAbandoned = "abandoned"
)

// Metadata keys stored on every mission's sessions.metadata JSON.
const (
	MetadataKeySkill        = "skill"
	MetadataKeyRole         = "role"
	MetadataKeyCoordSession = "coord_session_id"
	MetadataKeyDependsOn    = "depends_on"
	MetadataKeyAsyncHint    = "async_hint"
)

// PlannerMission is one node from the planner's LLM output, pre-
// persistence. IDs are planner-assigned 1..N integers; the executor
// allocates real session IDs when it seeds the in-memory DAG.
type PlannerMission struct {
	ID    int    `json:"id"`
	Skill string `json:"skill"`
	Role  string `json:"role"`
	Task  string `json:"task"`
}

// PlannerEdge is one "must complete before" relationship returned by
// the planner. From and To reference PlannerMission.ID values.
type PlannerEdge struct {
	From int `json:"from"`
	To   int `json:"to"`
}

// PlannerOutput is the strict JSON envelope the planner LLM returns.
type PlannerOutput struct {
	Missions []PlannerMission `json:"missions"`
	Edges    []PlannerEdge    `json:"edges"`
}

// PlanResult is the validated plan the planner returns. ChildIDs is
// filled in by the executor's Register after session-id allocation.
type PlanResult struct {
	Missions  []PlannerMission
	Edges     []PlannerEdge
	FromCache bool
	ChildIDs  map[int]string
}

// PlanOptions tune planner behaviour. Force bypasses the idempotency
// cache read (but the fresh plan is still written to the cache).
type PlanOptions struct {
	Force bool
}

// MissionRecord is the runtime view of one mission.
type MissionRecord struct {
	ID             string
	CoordSessionID string
	Skill          string
	Role           string
	Task           string
	Status         string
	DependsOn      []string
	TurnsUsed      int
	Summary        string
	Reason         string
	StartedAt      time.Time
	TerminatedAt   time.Time
}

// DispatchArgs are what the executor feeds to the mission driver.
type DispatchArgs struct {
	ParentSessionID string
	ChildSessionID  string
	CoordSessionID  string
	Skill           string
	Role            string
	Task            string
	Notes           string
	DependsOn       []string
	// InputArtifacts is the list of artifact ids the new mission
	// must be granted access to before its first turn. Visibility is
	// resolved against the coordinator session at promotion time —
	// a missing/invisible id fails the mission with an
	// `input_artifact_unknown_or_invisible: <id>` reason. Empty
	// slice = no auto-grant; the new mission sees only the default
	// `self`-scoped surface.
	InputArtifacts []string
}

// DispatchResult is what the mission driver returns.
type DispatchResult struct {
	Status       string
	Summary      string
	TurnsUsed    int
	DurationMs   int64
	Abstained    bool
	AbstainedWhy string
	Error        string
}

// CancelResult is what Executor.Cancel returns to the mission_cancel
// tool: the directly-cancelled mission, the cascade of dependents that
// got abandoned as a consequence (in BFS order), and the reason copy
// surfaced to the LLM envelope.
type CancelResult struct {
	Cancelled     string
	AlsoAbandoned []string
	Reason        string
}

// CompletionMarker is the verbatim user_message content the Executor
// emits when a coordinator's mission graph fully terminates. The
// coordinator's SKILL.md decision tree branch 8 keys off this marker
// to produce a single summary turn instead of a normal user-driven
// reply.
const CompletionMarker = "<system: missions complete>"

// MissionOutcome is one entry in the completion payload — the
// summarised result of a single mission, dehydrated for the
// coordinator's user-message metadata. Summary is the same text the
// agent_result event carried; Reason is set on failed/abandoned.
type MissionOutcome struct {
	MissionID string `json:"mission_id"`
	Skill     string `json:"skill"`
	Role      string `json:"role"`
	Status    string `json:"status"` // done | failed | abandoned
	Summary   string `json:"summary,omitempty"`
	Reason    string `json:"reason,omitempty"`
	TurnsUsed int    `json:"turns_used,omitempty"`
}

// CompletionPayload is the JSON shape attached to the synthetic
// user_message's metadata.completion_payload when a coordinator's
// graph terminates. AllSucceeded is true iff every Outcome is
// StatusDone.
type CompletionPayload struct {
	Outcomes     []MissionOutcome `json:"outcomes"`
	AllSucceeded bool             `json:"all_succeeded"`
}

// Sentinel errors surfaced by the planner / executor / store.
var (
	ErrPlanParse       = errors.New("missions: planner unparseable output")
	ErrUnknownRole     = errors.New("missions: planner references unknown (skill, role)")
	ErrUnknownEdgeNode = errors.New("missions: planner edge references unknown node")
	ErrDuplicateNode   = errors.New("missions: planner produced duplicate mission id")
	ErrCyclicGraph     = errors.New("missions: planner graph has cycle")
	ErrEmptyTask       = errors.New("missions: planner produced empty task")
	ErrNoMissions      = errors.New("missions: planner produced zero missions")

	ErrMissionNotFound = errors.New("missions: mission not found")
	ErrMissionTerminal = errors.New("missions: mission already terminal")
	ErrMissionNotOwned = errors.New("missions: mission not owned by this coordinator")

	ErrSpawnUnauthorised = errors.New("missions: role is not authorised to spawn")
	ErrSpawnDepth        = errors.New("missions: spawn depth limit reached")
)

// ValidatePlan runs Go-side invariant checks on a PlanResult before
// any persistence or DAG seeding.
func ValidatePlan(plan PlanResult) error {
	if len(plan.Missions) == 0 {
		return ErrNoMissions
	}
	ids := make(map[int]struct{}, len(plan.Missions))
	for _, m := range plan.Missions {
		if _, dup := ids[m.ID]; dup {
			return fmt.Errorf("%w: id %d", ErrDuplicateNode, m.ID)
		}
		if m.Task == "" {
			return ErrEmptyTask
		}
		ids[m.ID] = struct{}{}
	}
	for _, e := range plan.Edges {
		if e.From == e.To {
			return fmt.Errorf("%w: self-loop on %d", ErrCyclicGraph, e.From)
		}
		if _, ok := ids[e.From]; !ok {
			return fmt.Errorf("%w: edge from %d", ErrUnknownEdgeNode, e.From)
		}
		if _, ok := ids[e.To]; !ok {
			return fmt.Errorf("%w: edge to %d", ErrUnknownEdgeNode, e.To)
		}
	}
	if cycle := findCycle(plan.Missions, plan.Edges); cycle != 0 {
		return fmt.Errorf("%w: starting at %d", ErrCyclicGraph, cycle)
	}
	return nil
}

func findCycle(missions []PlannerMission, edges []PlannerEdge) int {
	adj := make(map[int][]int, len(missions))
	for _, m := range missions {
		adj[m.ID] = nil
	}
	for _, e := range edges {
		adj[e.From] = append(adj[e.From], e.To)
	}
	state := make(map[int]int, len(missions)) // 0 white, 1 grey, 2 black
	var visit func(int) int
	visit = func(n int) int {
		state[n] = 1
		for _, next := range adj[n] {
			switch state[next] {
			case 1:
				return next
			case 0:
				if h := visit(next); h != 0 {
					return h
				}
			}
		}
		state[n] = 2
		return 0
	}
	for _, m := range missions {
		if state[m.ID] == 0 {
			if h := visit(m.ID); h != 0 {
				return h
			}
		}
	}
	return 0
}
