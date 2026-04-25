// Package executor drives the in-memory mission DAG. One Executor
// per runtime; registered as a periodic task on pkg/scheduler, ticks
// every 2s. Each Tick reconciles terminal goroutines, cascades
// abandonment, promotes ready missions to running, and fires the
// completion-summary fan-out when a graph is fully terminal.
package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/hugr-lab/hugen/pkg/missions/graph"
	"github.com/hugr-lab/hugen/pkg/missions/store"
	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
)

// Executor drives the mission graph state machine. Holds an
// in-memory DAG per coordinator session; gates overlapping Tick
// calls via TryLock.
type Executor struct {
	store       *store.Store
	events      EventWriter
	driver      MissionDriver
	parallelism int
	staleAfter  time.Duration
	nowFn       func() time.Time
	logger      *slog.Logger

	// RunOnce, when set, is invoked by the completion-summary fan-out
	// (US4) to trigger exactly one coordinator turn after the graph
	// fully terminates. Nil => fan-out is skipped (US1-only wiring).
	RunOnce func(ctx context.Context, coordSessionID string) error

	// OnMissionReported, when set, is called by the dispatcher
	// goroutine AFTER it has written the DispatchResult to the
	// mission's internal terminal channel. Tests wire this for
	// deterministic synchronisation — once the hook fires, the next
	// Tick is guaranteed to drain this mission's terminal state. Nil
	// in production.
	OnMissionReported func(missionID string)

	tickMu sync.Mutex
	dags   sync.Map // coordSessionID → *dag
}

// Config bundles the Executor's construction dependencies.
type Config struct {
	Store       *store.Store
	Events      EventWriter
	Driver      MissionDriver
	Logger      *slog.Logger
	Parallelism int

	// StaleAfter caps how long an active mission can sit without
	// emitting an event before Restore considers it dead and marks
	// it abandoned. Zero falls back to 5 minutes (config default).
	StaleAfter time.Duration
}

// New builds an Executor. Parallelism < 1 is clamped to 4. Logger is
// nil-safe. All other fields are required.
func New(cfg Config) *Executor {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Parallelism < 1 {
		cfg.Parallelism = 4
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = 5 * time.Minute
	}
	return &Executor{
		store:       cfg.Store,
		events:      cfg.Events,
		driver:      cfg.Driver,
		parallelism: cfg.Parallelism,
		staleAfter:  cfg.StaleAfter,
		nowFn:       time.Now,
		logger:      cfg.Logger,
	}
}

// SetNowFn overrides the clock the Executor consults for staleness
// checks. Tests use this to align their notion of "now" with hub.db's
// server-stamped event timestamps; production never calls it.
func (e *Executor) SetNowFn(fn func() time.Time) {
	if fn == nil {
		fn = time.Now
	}
	e.nowFn = fn
}

// ------------------------------------------------------------------
// Public API
// ------------------------------------------------------------------

// Register seeds the in-memory DAG from a freshly-planned PlanResult.
// Call right after Planner.Plan succeeds — BEFORE any session row
// exists. Session IDs are generated here locally; they land in the
// hub only when the Executor promotes a mission to running and the
// Driver creates the session row.
//
// Mutates the passed PlanResult: fills in ChildIDs (planner-int →
// generated session id) so callers can surface the ids in their tool
// envelope response to the coordinator LLM.
func (e *Executor) Register(coordSessionID string, plan *graph.PlanResult) {
	if plan == nil || len(plan.Missions) == 0 {
		return
	}
	d := e.ensureDag(coordSessionID)
	d.mu.Lock()
	defer d.mu.Unlock()

	if plan.ChildIDs == nil {
		plan.ChildIDs = make(map[int]string, len(plan.Missions))
	}
	for _, m := range plan.Missions {
		sid := plan.ChildIDs[m.ID]
		if sid == "" {
			sid = "sub_" + uuid.NewString()
			plan.ChildIDs[m.ID] = sid
		}
		d.missions[sid] = &missionNode{
			id:      sid,
			coordID: coordSessionID,
			skill:   m.Skill,
			role:    m.Role,
			task:    m.Task,
			status:  graph.StatusPending,
		}
	}
	for _, edge := range plan.Edges {
		fromSID := plan.ChildIDs[edge.From]
		toSID := plan.ChildIDs[edge.To]
		if fromSID == "" || toSID == "" {
			continue
		}
		if to := d.missions[toSID]; to != nil {
			to.upstream = append(to.upstream, fromSID)
		}
		if from := d.missions[fromSID]; from != nil {
			from.downstream = append(from.downstream, toSID)
		}
	}
}

// Snapshot returns a stable read-only view of the coordinator's DAG,
// merging in-memory runtime status with persisted mission rows. Safe
// to call concurrently with Tick. When the in-memory DAG is empty
// (executor just booted, RestoreState hasn't run yet), falls back to
// a Store read — still returns missions, just without runtime
// granularity.
func (e *Executor) Snapshot(ctx context.Context, coordSessionID string) []graph.MissionRecord {
	if entry, ok := e.dags.Load(coordSessionID); ok {
		d := entry.(*dag)
		d.mu.Lock()
		defer d.mu.Unlock()
		out := make([]graph.MissionRecord, 0, len(d.missions))
		for _, n := range d.missions {
			out = append(out, graph.MissionRecord{
				ID:             n.id,
				CoordSessionID: coordSessionID,
				Skill:          n.skill,
				Role:           n.role,
				Task:           n.task,
				Status:         n.status,
				DependsOn:      append([]string(nil), n.upstream...),
				TurnsUsed:      n.turnsUsed,
				Summary:        n.summary,
				Reason:         n.reason,
				StartedAt:      n.startedAt,
				TerminatedAt:   n.terminated,
			})
		}
		return out
	}
	ms, err := e.store.ListMissions(ctx, coordSessionID, "")
	if err != nil {
		e.logger.WarnContext(ctx, "missions: snapshot fallback", "coord", coordSessionID, "err", err)
		return nil
	}
	return ms
}

// RunningMissions returns the subset of this coordinator's DAG in
// StatusRunning. Empty slice when the coordinator has no in-memory
// DAG. Used by the follow-up router to decide whether a user message
// could plausibly be refining an in-flight mission.
func (e *Executor) RunningMissions(coordSessionID string) []graph.MissionRecord {
	entry, ok := e.dags.Load(coordSessionID)
	if !ok {
		return nil
	}
	d := entry.(*dag)
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []graph.MissionRecord
	for _, n := range d.missions {
		if n.status != graph.StatusRunning {
			continue
		}
		out = append(out, graph.MissionRecord{
			ID:             n.id,
			CoordSessionID: coordSessionID,
			Skill:          n.skill,
			Role:           n.role,
			Task:           n.task,
			Status:         n.status,
			DependsOn:      append([]string(nil), n.upstream...),
			TurnsUsed:      n.turnsUsed,
			StartedAt:      n.startedAt,
		})
	}
	return out
}

// OnFollowUp appends a user message as a new turn in the target
// mission's session AND writes the audit trail on the coordinator.
// FR-013: the refinement joins the child transcript naturally (next
// dispatcher turn sees it as a user_message) and the coordinator
// keeps a queryable record of where the route landed.
func (e *Executor) OnFollowUp(
	ctx context.Context,
	coordSessionID, userMsg, targetMissionID string,
	confidence float64,
) error {
	if targetMissionID == "" {
		return fmt.Errorf("missions: follow-up target mission id is empty")
	}
	if strings.TrimSpace(userMsg) == "" {
		return fmt.Errorf("missions: follow-up user message is empty")
	}
	if _, err := e.events.AppendEvent(ctx, sessstore.Event{
		SessionID: targetMissionID,
		EventType: sessstore.EventTypeUserMessage,
		Author:    "user",
		Content:   userMsg,
	}); err != nil {
		return fmt.Errorf("missions: append follow-up to mission %s: %w", targetMissionID, err)
	}
	meta := map[string]any{
		"target_mission_id":     targetMissionID,
		"classifier_confidence": confidence,
	}
	if _, err := e.events.AppendEvent(ctx, sessstore.Event{
		SessionID: coordSessionID,
		EventType: sessstore.EventTypeUserFollowupRouted,
		Author:    "user",
		Content:   truncate(userMsg, 2048),
		Metadata:  meta,
	}); err != nil {
		// Best effort — routing already succeeded on the child side.
		e.logger.WarnContext(ctx, "missions: emit user_followup_routed",
			"coord", coordSessionID, "err", err)
	}
	return nil
}

// RestoreReport is the summary RestoreState returns so the runtime
// can log a single line on boot describing what survived the
// restart.
type RestoreReport struct {
	Coordinators   int // distinct coords whose DAG was rebuilt
	Resumed        int // missions reattached as running
	Pending        int // missions still pending (deps unsatisfied)
	Ready          int // missions promoted to ready (all deps done)
	StaleAbandoned int // active rows whose last event was older than StaleAfter
}

// RestoreState rebuilds every coordinator's DAG from hub.db on boot.
// Loads ALL sub-agent rows (including terminal) so pending missions
// can correctly evaluate their upstream done-ness; for each non-
// terminal row, freshness-checks the last event timestamp and
// abandons stale ones (FR-019). Active rows whose last event is
// fresh are reattached as `running` — no `mission_spawn` event is
// re-emitted (FR-020), spec-006's transcript replay path resumes
// the model.
//
// Caller must invoke once before the scheduler kicks off the first
// Tick. Concurrent Restore + Tick is undefined behaviour.
func (e *Executor) RestoreState(ctx context.Context) (RestoreReport, error) {
	rows, err := e.store.ListAllAgentMissions(ctx)
	if err != nil {
		return RestoreReport{}, fmt.Errorf("missions: restore — list: %w", err)
	}

	byCoord := map[string][]graph.MissionRecord{}
	for _, r := range rows {
		if r.CoordSessionID == "" {
			continue
		}
		byCoord[r.CoordSessionID] = append(byCoord[r.CoordSessionID], r)
	}

	var rep RestoreReport
	now := e.nowFn()
	for coord, missions := range byCoord {
		rep.Coordinators++
		d := e.ensureDag(coord)
		d.mu.Lock()

		for _, m := range missions {
			node := &missionNode{
				id:         m.ID,
				coordID:    coord,
				skill:      m.Skill,
				role:       m.Role,
				task:       m.Task,
				status:     m.Status,
				upstream:   append([]string(nil), m.DependsOn...),
				summary:    m.Summary,
				reason:     m.Reason,
				startedAt:  m.StartedAt,
				terminated: m.TerminatedAt,
			}

			// Active in hub → either resume or stale-abandon.
			if m.Status == graph.StatusRunning {
				last, lastErr := e.store.LastEventAt(ctx, m.ID)
				stale := lastErr == nil && !last.IsZero() && now.Sub(last) > e.staleAfter
				if stale {
					node.status = graph.StatusAbandoned
					node.terminated = now
					node.reason = "restart: stale"
					if err := e.store.MarkStatus(ctx, m.ID, graph.StatusAbandoned); err != nil {
						e.logger.WarnContext(ctx, "missions: restore mark stale",
							"id", m.ID, "err", err)
					}
					e.emitResult(ctx, node, missionResult{
						status:   graph.StatusAbandoned,
						errorMsg: "restart: stale",
					})
					rep.StaleAbandoned++
				} else {
					rep.Resumed++
				}
			}
			d.missions[m.ID] = node
		}

		// Wire downstream slots from upstream so the cascade walk +
		// promoteReady have full graph visibility.
		for _, n := range d.missions {
			for _, up := range n.upstream {
				if upN, ok := d.missions[up]; ok {
					upN.downstream = append(upN.downstream, n.id)
				}
			}
		}

		// Recompute pending → ready / abandoned based on persisted
		// upstream statuses. Run AFTER edge wiring so the dep lookup
		// sees terminal upstreams.
		for _, n := range d.missions {
			if n.status != graph.StatusPending {
				continue
			}
			var (
				allDone     = true
				anyTerminal bool
			)
			for _, up := range n.upstream {
				upN, ok := d.missions[up]
				if !ok {
					// Upstream missing → treat as terminal-failed
					// (edge of the DAG we don't see). Conservative.
					anyTerminal = true
					allDone = false
					break
				}
				switch upN.status {
				case graph.StatusFailed, graph.StatusAbandoned:
					anyTerminal = true
					allDone = false
				case graph.StatusDone:
					// keep allDone
				default:
					allDone = false
				}
			}
			switch {
			case anyTerminal:
				n.status = graph.StatusAbandoned
				n.reason = "restart: upstream terminal"
				rep.StaleAbandoned++
				if err := e.store.MarkStatus(ctx, n.id, graph.StatusAbandoned); err != nil {
					e.logger.WarnContext(ctx, "missions: restore cascade",
						"id", n.id, "err", err)
				}
				e.emitResult(ctx, n, missionResult{
					status:   graph.StatusAbandoned,
					errorMsg: n.reason,
				})
			case allDone:
				n.status = graph.StatusReady
				rep.Ready++
			default:
				rep.Pending++
			}
		}
		d.mu.Unlock()
	}

	e.logger.Info("missions: restore complete",
		"coordinators", rep.Coordinators,
		"resumed", rep.Resumed,
		"ready", rep.Ready,
		"pending", rep.Pending,
		"stale_abandoned", rep.StaleAbandoned)
	return rep, nil
}

// Cancel marks `missionID` abandoned and walks downstream to abandon
// every dependent. Holds tickMu (full lock — waits) so a concurrent
// Tick can't promote a dependent into running while the cascade is
// being written.
//
// On the cancelled mission: if it was already running, its per-mission
// ctx is cancelled (driver goroutine exits at next turn boundary) and
// resultCh is dropped so drainTerminals won't double-emit a
// mission_result. If the mission never started, the row is created
// directly in terminal abandoned state.
//
// Returns ErrMissionNotFound when no DAG holds this id, and
// ErrMissionTerminal when the mission is already in a terminal state
// (with a wrapped status for the LLM envelope).
func (e *Executor) Cancel(ctx context.Context, missionID string) (graph.CancelResult, error) {
	e.tickMu.Lock()
	defer e.tickMu.Unlock()

	d, node := e.findNode(missionID)
	if node == nil {
		return graph.CancelResult{}, graph.ErrMissionNotFound
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	switch node.status {
	case graph.StatusDone, graph.StatusFailed, graph.StatusAbandoned:
		return graph.CancelResult{}, fmt.Errorf("%w: %s is %s",
			graph.ErrMissionTerminal, missionID, node.status)
	}

	reason := "cancelled by user"
	e.abandonNode(ctx, node, reason)

	var alsoAbandoned []string
	queue := append([]string(nil), node.downstream...)
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		dep := d.missions[next]
		if dep == nil {
			continue
		}
		switch dep.status {
		case graph.StatusDone, graph.StatusFailed, graph.StatusAbandoned:
			continue
		}
		e.abandonNode(ctx, dep, "upstream cancelled: "+missionID)
		alsoAbandoned = append(alsoAbandoned, next)
		queue = append(queue, dep.downstream...)
	}

	return graph.CancelResult{
		Cancelled:     missionID,
		AlsoAbandoned: alsoAbandoned,
		Reason:        reason,
	}, nil
}

// AbandonCoordinator fans out Cancel over every non-terminal mission
// belonging to coordSessionID. Wired to SessionManager.OnSessionClose
// so closing a coordinator drains any in-flight missions.
//
// Errors from a per-mission Cancel are logged but never propagated:
// the cascade pass marks dependents terminal, so subsequent Cancels
// silently no-op via ErrMissionTerminal.
func (e *Executor) AbandonCoordinator(ctx context.Context, coordSessionID string) {
	entry, ok := e.dags.Load(coordSessionID)
	if !ok {
		return
	}
	d := entry.(*dag)
	d.mu.Lock()
	var ids []string
	for id, n := range d.missions {
		switch n.status {
		case graph.StatusDone, graph.StatusFailed, graph.StatusAbandoned:
			continue
		default:
			ids = append(ids, id)
		}
	}
	d.mu.Unlock()
	for _, id := range ids {
		if _, err := e.Cancel(ctx, id); err != nil {
			if errors.Is(err, graph.ErrMissionTerminal) {
				continue
			}
			e.logger.WarnContext(ctx, "missions: abandon coordinator cancel",
				"coord", coordSessionID, "id", id, "err", err)
		}
	}
}

// abandonNode is the per-node terminal write shared by Cancel and the
// cascade walk. Caller MUST hold d.mu and tickMu. Idempotency
// (already-terminal short-circuit) is the caller's responsibility.
func (e *Executor) abandonNode(ctx context.Context, node *missionNode, reason string) {
	node.status = graph.StatusAbandoned
	node.terminated = time.Now()
	node.reason = reason
	if node.cancel != nil {
		node.cancel()
		node.cancel = nil
	}
	// Drop the result channel so drainTerminals won't double-emit when
	// the dispatcher goroutine eventually publishes its own terminal
	// result — the channel is buffered, the orphan write just drops.
	node.resultCh = nil

	if node.startedAt.IsZero() {
		if err := e.store.RecordAbandoned(ctx,
			node.id, node.coordID, node.coordID,
			node.skill, node.role, node.task,
			append([]string(nil), node.upstream...),
			reason,
		); err != nil {
			e.logger.WarnContext(ctx, "missions: record abandoned on cancel",
				"id", node.id, "err", err)
		}
	} else {
		if err := e.store.MarkStatus(ctx, node.id, graph.StatusAbandoned); err != nil {
			e.logger.WarnContext(ctx, "missions: mark abandoned on cancel",
				"id", node.id, "err", err)
		}
	}
	e.emitResult(ctx, node, missionResult{
		status:    graph.StatusAbandoned,
		errorMsg:  reason,
		turnsUsed: node.turnsUsed,
	})
}

// findNode locates a mission by id across every coordinator's DAG.
// Returns (nil, nil) when no DAG holds this id. The returned dag's
// mutex is NOT held — callers re-acquire it before mutating state.
func (e *Executor) findNode(missionID string) (*dag, *missionNode) {
	var (
		foundD *dag
		foundN *missionNode
	)
	e.dags.Range(func(_, v any) bool {
		d := v.(*dag)
		d.mu.Lock()
		if n, ok := d.missions[missionID]; ok {
			foundD = d
			foundN = n
			d.mu.Unlock()
			return false
		}
		d.mu.Unlock()
		return true
	})
	return foundD, foundN
}

// Tick reconciles every coordinator's DAG: drain terminal goroutines
// → cascade abandonment → pending→ready → ready→running → completion
// summary. Guarded by TryLock — a concurrent tick short-circuits with
// a DEBUG log.
func (e *Executor) Tick(ctx context.Context) {
	if !e.tickMu.TryLock() {
		e.logger.DebugContext(ctx, "missions: tick skipped (prior tick still running)")
		return
	}
	defer e.tickMu.Unlock()

	e.dags.Range(func(k, v any) bool {
		coordID := k.(string)
		d := v.(*dag)
		e.tickDag(ctx, coordID, d)
		return true
	})
}

// ------------------------------------------------------------------
// Tick body
// ------------------------------------------------------------------

func (e *Executor) tickDag(ctx context.Context, coordID string, d *dag) {
	terminals := e.drainTerminals(ctx, d)
	if len(terminals) > 0 {
		e.cascadeAbandon(ctx, d, terminals)
	}
	e.promoteReady(d)
	e.promoteRunning(ctx, coordID, d)
	e.maybeCompletionSummary(ctx, coordID, d, terminals)
}

// drainTerminals collects any mission whose Dispatcher goroutine has
// finished and applies the terminal status + emits agent_result.
func (e *Executor) drainTerminals(ctx context.Context, d *dag) []string {
	var terminals []string
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, node := range d.missions {
		if node.status != graph.StatusRunning || node.resultCh == nil {
			continue
		}
		select {
		case res := <-node.resultCh:
			node.resultCh = nil
			node.cancel = nil
			node.terminated = time.Now()
			node.turnsUsed = res.turnsUsed
			node.summary = res.summary
			node.reason = res.errorMsg
			switch res.status {
			case graph.StatusDone, graph.StatusFailed, graph.StatusAbandoned:
				node.status = res.status
			default:
				node.status = graph.StatusDone
			}
			if err := e.store.MarkStatus(ctx, id, node.status); err != nil {
				e.logger.WarnContext(ctx, "missions: mark status", "id", id, "err", err)
			}
			e.emitResult(ctx, node, res)
			if res.abstained {
				e.emitAbstained(ctx, node, res.abstainedWhy)
			}
			terminals = append(terminals, id)
		default:
			// still running
		}
	}
	return terminals
}

// cascadeAbandon walks downstream from each failed/abandoned terminal
// and marks dependents abandoned. Creates rows for missions that
// never got the chance to run (upstream failed before promotion).
func (e *Executor) cascadeAbandon(ctx context.Context, d *dag, terminals []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, id := range terminals {
		src := d.missions[id]
		if src == nil {
			continue
		}
		if src.status != graph.StatusFailed && src.status != graph.StatusAbandoned {
			continue
		}
		queue := append([]string(nil), src.downstream...)
		for len(queue) > 0 {
			next := queue[0]
			queue = queue[1:]
			node := d.missions[next]
			if node == nil {
				continue
			}
			if node.status == graph.StatusDone ||
				node.status == graph.StatusFailed ||
				node.status == graph.StatusAbandoned {
				continue
			}
			node.status = graph.StatusAbandoned
			node.terminated = time.Now()
			node.reason = fmt.Sprintf("upstream %s: %s", src.status, id)
			if node.startedAt.IsZero() {
				// Never started — create the row directly in terminal.
				if err := e.store.RecordAbandoned(ctx,
					next, node.coordID, node.coordID,
					node.skill, node.role, node.task,
					append([]string(nil), node.upstream...),
					node.reason,
				); err != nil {
					e.logger.WarnContext(ctx, "missions: record abandoned", "id", next, "err", err)
				}
			} else {
				if err := e.store.MarkStatus(ctx, next, graph.StatusAbandoned); err != nil {
					e.logger.WarnContext(ctx, "missions: mark abandoned", "id", next, "err", err)
				}
			}
			e.emitResult(ctx, node, missionResult{
				status:    graph.StatusAbandoned,
				errorMsg:  node.reason,
				turnsUsed: 0,
			})
			queue = append(queue, node.downstream...)
		}
	}
}

// promoteReady flips pending missions to ready when every upstream
// mission has reached StatusDone.
func (e *Executor) promoteReady(d *dag) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, node := range d.missions {
		if node.status != graph.StatusPending {
			continue
		}
		allDone := true
		for _, up := range node.upstream {
			upstream := d.missions[up]
			if upstream == nil || upstream.status != graph.StatusDone {
				allDone = false
				break
			}
		}
		if allDone {
			node.status = graph.StatusReady
		}
	}
}

// promoteRunning launches dispatcher goroutines for ready missions,
// bounded by Parallelism. Goroutines write their DispatchResult into
// per-node channels drained by the next Tick's drainTerminals.
func (e *Executor) promoteRunning(ctx context.Context, coordID string, d *dag) {
	d.mu.Lock()
	defer d.mu.Unlock()

	inFlight := 0
	for _, n := range d.missions {
		if n.status == graph.StatusRunning {
			inFlight++
		}
	}
	if inFlight >= e.parallelism {
		return
	}

	for _, node := range d.missions {
		if node.status != graph.StatusReady {
			continue
		}
		if inFlight >= e.parallelism {
			break
		}
		node.status = graph.StatusRunning
		node.startedAt = time.Now()
		node.resultCh = make(chan missionResult, 1)
		evtID := e.emitSpawn(ctx, coordID, node)
		node.spawnEventID = evtID

		runCtx, cancel := context.WithCancel(context.Background())
		node.cancel = cancel
		args := graph.DispatchArgs{
			ParentSessionID: coordID,
			ChildSessionID:  node.id,
			CoordSessionID:  coordID,
			Skill:           node.skill,
			Role:            node.role,
			Task:            node.task,
			DependsOn:       append([]string(nil), node.upstream...),
		}
		ch := node.resultCh
		driver := e.driver
		hook := e.OnMissionReported
		missionID := node.id
		go func() {
			defer cancel()
			res := driver.RunMission(runCtx, args)
			ch <- missionResult{
				status:       res.Status,
				summary:      res.Summary,
				turnsUsed:    res.TurnsUsed,
				durationMs:   res.DurationMs,
				abstained:    res.Abstained,
				abstainedWhy: res.AbstainedWhy,
				errorMsg:     res.Error,
			}
			if hook != nil {
				hook(missionID)
			}
		}()
		inFlight++
	}
}

// maybeCompletionSummary fires exactly once per graph when every
// mission reaches a terminal status AND the latest tick caused the
// final transition. Emits a synthetic user_message on the coordinator
// session carrying the structured completion payload in metadata, so
// the next coordinator turn (whether user-driven or RunOnce-driven)
// reads `<system: missions complete>` in its prompt and produces the
// SKILL.md branch-8 summary reply. When RunOnce is set, the executor
// also fires it asynchronously to drive that turn without waiting on
// the user.
func (e *Executor) maybeCompletionSummary(
	ctx context.Context,
	coordID string,
	d *dag,
	terminals []string,
) {
	if len(terminals) == 0 {
		return
	}
	d.mu.Lock()
	pending := false
	for _, n := range d.missions {
		if n.status == graph.StatusPending ||
			n.status == graph.StatusReady ||
			n.status == graph.StatusRunning {
			pending = true
			break
		}
	}
	alreadyFired := d.completionFired
	if !pending && !alreadyFired {
		d.completionFired = true
	}
	payload := graph.CompletionPayload{}
	if !pending && !alreadyFired {
		payload.AllSucceeded = true
		for _, n := range d.missions {
			outcome := graph.MissionOutcome{
				MissionID: n.id,
				Skill:     n.skill,
				Role:      n.role,
				Status:    n.status,
				Summary:   n.summary,
				Reason:    n.reason,
				TurnsUsed: n.turnsUsed,
			}
			if n.status != graph.StatusDone {
				payload.AllSucceeded = false
			}
			payload.Outcomes = append(payload.Outcomes, outcome)
		}
	}
	d.mu.Unlock()

	if pending || alreadyFired {
		return
	}

	if _, err := e.events.AppendEvent(ctx, sessstore.Event{
		SessionID: coordID,
		EventType: sessstore.EventTypeUserMessage,
		Author:    "user",
		Content:   graph.CompletionMarker,
		Metadata:  map[string]any{"completion_payload": payload},
	}); err != nil {
		e.logger.WarnContext(ctx, "missions: completion marker", "coord", coordID, "err", err)
	}

	if e.RunOnce == nil {
		return
	}
	go func() {
		if err := e.RunOnce(ctx, coordID); err != nil {
			e.logger.WarnContext(ctx, "missions: completion summary", "coord", coordID, "err", err)
		}
	}()
}

func (e *Executor) ensureDag(coordID string) *dag {
	if existing, ok := e.dags.Load(coordID); ok {
		return existing.(*dag)
	}
	fresh := &dag{coordID: coordID, missions: map[string]*missionNode{}}
	actual, _ := e.dags.LoadOrStore(coordID, fresh)
	return actual.(*dag)
}
