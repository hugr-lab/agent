package missions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"

	"github.com/hugr-lab/hugen/pkg/missions/graph"
	"github.com/hugr-lab/hugen/pkg/sessions"
	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
	"github.com/hugr-lab/hugen/pkg/skills"
	"github.com/hugr-lab/hugen/pkg/tools"
)

// ------------------------------------------------------------------
// mission_plan
// ------------------------------------------------------------------

type missionPlanTool struct {
	svc *Service
}

func (t *missionPlanTool) Name() string { return "mission_plan" }
func (t *missionPlanTool) Description() string {
	return "Decomposes a multi-step user goal into a dependency graph of specialist sub-agents. " +
		"Persists every mission + edge, then returns the plan. Does NOT start the missions — " +
		"the scheduler promotes them on the next tick. Use only when the goal actually requires " +
		"multiple steps; a single narrow task should go directly to subagent_<skill>_<role>."
}
func (t *missionPlanTool) IsLongRunning() bool { return false }

func (t *missionPlanTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: "OBJECT",
			Properties: map[string]*genai.Schema{
				"goal": {
					Type:        "STRING",
					Description: "The user's multi-step goal. Keep it self-contained — the planner LLM does not see the conversation history.",
				},
				"force": {
					Type:        "BOOLEAN",
					Description: "Bypass the idempotency cache. Default false. Set true only if a prior identical goal produced a stale plan after context changes.",
				},
			},
			Required: []string{"goal"},
		},
	}
}

func (t *missionPlanTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *missionPlanTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	m, ok := args.(map[string]any)
	if !ok {
		return errorEnvelope("mission_plan: unexpected args"), nil
	}
	goal, _ := m["goal"].(string)
	if strings.TrimSpace(goal) == "" {
		return errorEnvelope("goal is required"), nil
	}
	force, _ := m["force"].(bool)

	sess, err := sessionFromContext(ctx, t.svc.sessions)
	if err != nil {
		return errorEnvelope(err.Error()), nil
	}
	loaded, err := loadActiveSkills(ctx, t.svc.skills, sess)
	if err != nil {
		return errorEnvelope("failed to read active skills: " + err.Error()), nil
	}

	plan, err := t.svc.planner.Plan(ctx, sess.ID(), goal, loaded, graph.PlanOptions{Force: force})
	if err != nil {
		return planErrorEnvelope(err), nil
	}
	if plan.FromCache {
		return planEnvelope(plan, true), nil
	}
	t.svc.executor.Register(sess.ID(), &plan)
	return planEnvelope(plan, false), nil
}

// ------------------------------------------------------------------
// mission_status
// ------------------------------------------------------------------

type missionStatusTool struct {
	svc *Service
}

func (t *missionStatusTool) Name() string { return "mission_status" }
func (t *missionStatusTool) Description() string {
	return "Returns the current mission graph for this coordinator session — one tree line per " +
		"mission, tagged with live status. Call when the user asks 'how's it going?' or before " +
		"announcing completion."
}
func (t *missionStatusTool) IsLongRunning() bool { return false }

func (t *missionStatusTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters:  &genai.Schema{Type: "OBJECT", Properties: map[string]*genai.Schema{}},
	}
}

func (t *missionStatusTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *missionStatusTool) Run(ctx tool.Context, _ any) (map[string]any, error) {
	sess, err := sessionFromContext(ctx, t.svc.sessions)
	if err != nil {
		return errorEnvelope(err.Error()), nil
	}
	nodes := t.svc.executor.Snapshot(ctx, sess.ID())
	tree := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		tree = append(tree, map[string]any{
			"mission_id": n.ID,
			"skill":      n.Skill,
			"role":       n.Role,
			"task":       n.Task,
			"status":     n.Status,
			"turns_used": n.TurnsUsed,
			"depends_on": n.DependsOn,
			"summary":    n.Summary,
		})
	}
	return map[string]any{
		"tree":     tree,
		"rendered": renderTree(nodes),
	}, nil
}

// ------------------------------------------------------------------
// mission_cancel
// ------------------------------------------------------------------

type missionCancelTool struct {
	svc *Service
}

func (t *missionCancelTool) Name() string { return "mission_cancel" }
func (t *missionCancelTool) Description() string {
	return "Cancels a running or pending mission identified by its session id. Walks the dependency " +
		"graph and abandons every dependent. Use when the user changes their mind, when an in-flight " +
		"mission is taking the wrong direction, or before announcing completion if a mission is stuck."
}
func (t *missionCancelTool) IsLongRunning() bool { return false }

func (t *missionCancelTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: "OBJECT",
			Properties: map[string]*genai.Schema{
				"mission_id": {
					Type:        "STRING",
					Description: "The session id of the mission to cancel — appears as `mission_id` in mission_plan and mission_status output.",
				},
				"reason": {
					Type:        "STRING",
					Description: "Optional human-readable note shown alongside the cancellation in audit logs.",
				},
			},
			Required: []string{"mission_id"},
		},
	}
}

func (t *missionCancelTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *missionCancelTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	m, ok := args.(map[string]any)
	if !ok {
		return errorEnvelope("mission_cancel: unexpected args"), nil
	}
	missionID, _ := m["mission_id"].(string)
	if strings.TrimSpace(missionID) == "" {
		return errorEnvelope("mission_id is required"), nil
	}

	sess, err := sessionFromContext(ctx, t.svc.sessions)
	if err != nil {
		return errorEnvelope(err.Error()), nil
	}

	// Visibility gate: the mission must belong to this coordinator's
	// DAG. Snapshot is the canonical source — Cancel itself doesn't
	// scope by coord, so we enforce the boundary at the tool layer.
	if !t.coordOwnsMission(ctx, sess.ID(), missionID) {
		return errorEnvelope("mission not found"), nil
	}

	res, err := t.svc.executor.Cancel(ctx, missionID)
	if err != nil {
		switch {
		case errors.Is(err, graph.ErrMissionNotFound):
			return errorEnvelope("mission not found"), nil
		case errors.Is(err, graph.ErrMissionTerminal):
			return errorEnvelope(err.Error()), nil
		default:
			return errorEnvelope("mission_cancel: " + err.Error()), nil
		}
	}

	if res.AlsoAbandoned == nil {
		res.AlsoAbandoned = []string{}
	}
	return map[string]any{
		"cancelled":      res.Cancelled,
		"also_abandoned": res.AlsoAbandoned,
		"reason":         res.Reason,
	}, nil
}

// coordOwnsMission is a best-effort visibility check — it reads
// Snapshot under dag.mu, then the caller invokes Cancel which
// re-acquires the lock. Between those two critical sections the
// mission may finish; Cancel surfaces ErrMissionTerminal and the
// tool layer maps it to a clean error envelope.
func (t *missionCancelTool) coordOwnsMission(ctx context.Context, coordID, missionID string) bool {
	for _, n := range t.svc.executor.Snapshot(ctx, coordID) {
		if n.ID == missionID {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------------
// mission_sub_runs
// ------------------------------------------------------------------

type missionSubRunsTool struct {
	svc *Service
}

func (t *missionSubRunsTool) Name() string { return "mission_sub_runs" }
func (t *missionSubRunsTool) Description() string {
	return "Returns the last N transcript events of a mission's own session — tool calls + results " +
		"summarised, llm_response excerpts. Use to answer 'what is mission X doing right now?' " +
		"without dumping the full transcript into your own context."
}
func (t *missionSubRunsTool) IsLongRunning() bool { return false }

func (t *missionSubRunsTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: "OBJECT",
			Properties: map[string]*genai.Schema{
				"mission_id": {
					Type:        "STRING",
					Description: "The mission's session id (from mission_plan / mission_status).",
				},
				"limit": {
					Type:        "INTEGER",
					Description: "How many trailing events to return. Default 20, maximum 50.",
				},
			},
			Required: []string{"mission_id"},
		},
	}
}

func (t *missionSubRunsTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *missionSubRunsTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	m, ok := args.(map[string]any)
	if !ok {
		return errorEnvelope("mission_sub_runs: unexpected args"), nil
	}
	missionID, _ := m["mission_id"].(string)
	if strings.TrimSpace(missionID) == "" {
		return errorEnvelope("mission_id is required"), nil
	}

	limit := subRunsLimit(m["limit"])

	sess, err := sessionFromContext(ctx, t.svc.sessions)
	if err != nil {
		return errorEnvelope(err.Error()), nil
	}
	if !subRunsCoordOwns(ctx, t.svc.executor, sess.ID(), missionID) {
		return errorEnvelope("mission not visible from this coordinator"), nil
	}

	if t.svc.events == nil {
		return errorEnvelope("event reader not configured"), nil
	}
	events, err := t.svc.events.GetEvents(ctx, missionID)
	if err != nil {
		return errorEnvelope("failed to read mission events: " + err.Error()), nil
	}

	tailStart := len(events) - limit
	truncated := tailStart > 0
	if tailStart < 0 {
		tailStart = 0
	}
	tail := events[tailStart:]

	out := make([]map[string]any, 0, len(tail))
	for _, ev := range tail {
		entry := map[string]any{
			"seq":        ev.Seq,
			"event_type": ev.EventType,
		}
		switch ev.EventType {
		case sessstore.EventTypeToolCall:
			entry["tool_name"] = ev.ToolName
			entry["args_digest"] = digestArgs(ev.ToolArgs, 200)
		case sessstore.EventTypeToolResult:
			entry["tool_name"] = ev.ToolName
			entry["result_digest"] = digestString(ev.ToolResult, 200)
		default:
			entry["content"] = digestString(ev.Content, 500)
		}
		out = append(out, entry)
	}

	return map[string]any{
		"mission_id": missionID,
		"events":     out,
		"truncated":  truncated,
	}, nil
}

func subRunsLimit(raw any) int {
	const (
		def = 20
		max = 50
	)
	switch v := raw.(type) {
	case nil:
		return def
	case float64:
		if v <= 0 {
			return def
		}
		n := int(v)
		if n > max {
			return max
		}
		return n
	case int:
		if v <= 0 {
			return def
		}
		if v > max {
			return max
		}
		return v
	default:
		return def
	}
}

func subRunsCoordOwns(ctx context.Context, exec missionSnapshotter, coordID, missionID string) bool {
	for _, n := range exec.Snapshot(ctx, coordID) {
		if n.ID == missionID {
			return true
		}
	}
	return false
}

// missionSnapshotter is the executor surface mission_sub_runs needs —
// kept narrow so unit tests can swap a fake snapshot without spinning
// up an Executor.
type missionSnapshotter interface {
	Snapshot(ctx context.Context, coordSessionID string) []graph.MissionRecord
}

func digestArgs(args map[string]any, n int) string {
	if len(args) == 0 {
		return ""
	}
	b, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return digestString(string(b), n)
}

func digestString(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ------------------------------------------------------------------
// Shared helpers
// ------------------------------------------------------------------

func errorEnvelope(msg string) map[string]any { return map[string]any{"error": msg} }

func planEnvelope(plan graph.PlanResult, fromCache bool) map[string]any {
	missionObjs := make([]map[string]any, 0, len(plan.Missions))
	for _, m := range plan.Missions {
		obj := map[string]any{
			"id":    m.ID,
			"skill": m.Skill,
			"role":  m.Role,
			"task":  m.Task,
		}
		if plan.ChildIDs != nil {
			if sid, ok := plan.ChildIDs[m.ID]; ok {
				obj["mission_id"] = sid
			}
		}
		missionObjs = append(missionObjs, obj)
	}
	edgeObjs := make([]map[string]any, 0, len(plan.Edges))
	for _, e := range plan.Edges {
		edgeObjs = append(edgeObjs, map[string]any{"from": e.From, "to": e.To})
	}
	return map[string]any{
		"missions_planned": len(plan.Missions),
		"graph":            map[string]any{"missions": missionObjs, "edges": edgeObjs},
		"from_cache":       fromCache,
		"hint":             "Call mission_status() any time to see progress. I will announce completion.",
	}
}

func planErrorEnvelope(err error) map[string]any {
	switch {
	case errors.Is(err, graph.ErrPlanParse):
		return errorEnvelope("planner returned unparseable output")
	case errors.Is(err, graph.ErrNoMissions):
		return errorEnvelope("planner produced empty graph")
	case errors.Is(err, graph.ErrUnknownRole):
		return errorEnvelope("planner referenced an unknown (skill, role)")
	case errors.Is(err, graph.ErrCyclicGraph):
		return errorEnvelope("planner graph has a cycle")
	case errors.Is(err, graph.ErrDuplicateNode):
		return errorEnvelope("planner produced duplicate mission ids")
	case errors.Is(err, graph.ErrEmptyTask):
		return errorEnvelope("planner produced a mission with an empty task")
	case errors.Is(err, graph.ErrUnknownEdgeNode):
		return errorEnvelope("planner edge references an unknown mission")
	case errors.Is(err, context.DeadlineExceeded):
		return errorEnvelope("planner timed out")
	default:
		return map[string]any{"error": "planner failed", "detail": err.Error()}
	}
}

// renderTree formats the mission nodes into the indented human-readable
// tree the coordinator pastes into its reply. Letters A, B, C, … are
// assigned by topological order (roots first); status is uppercased.
func renderTree(nodes []graph.MissionRecord) string {
	if len(nodes) == 0 {
		return "No missions running."
	}
	sorted := append([]graph.MissionRecord(nil), nodes...)
	sort.Slice(sorted, func(i, j int) bool {
		if len(sorted[i].DependsOn) != len(sorted[j].DependsOn) {
			return len(sorted[i].DependsOn) < len(sorted[j].DependsOn)
		}
		return sorted[i].ID < sorted[j].ID
	})
	var b strings.Builder
	for i, n := range sorted {
		letter := string(rune('A' + i%26))
		task := n.Task
		if len(task) > 40 {
			task = task[:37] + "…"
		}
		statusUpper := strings.ToUpper(n.Status)
		extras := ""
		if n.Status == graph.StatusRunning && n.TurnsUsed > 0 {
			extras = fmt.Sprintf(" (turn %d)", n.TurnsUsed)
		}
		fmt.Fprintf(&b, "%s %s(%s)  %s%s\n", letter, n.Role, task, statusUpper, extras)
	}
	return strings.TrimRight(b.String(), "\n")
}

// sessionFromContext resolves the current session from tool.Context.
func sessionFromContext(ctx tool.Context, sm *sessions.Manager) (*sessions.Session, error) {
	if sm == nil {
		return nil, fmt.Errorf("missions: session manager is nil")
	}
	sid := ctx.SessionID()
	if sid == "" {
		return nil, fmt.Errorf("missions: no session id in tool context")
	}
	sess, err := sm.Session(sid)
	if err != nil {
		return nil, fmt.Errorf("missions: resolve session: %w", err)
	}
	return sess, nil
}

// loadActiveSkills walks the current session's active-skill names and
// loads each via skills.Manager. Cost is proportional to number of
// loaded skills (typically 3–5); acceptable on the planner path.
func loadActiveSkills(ctx context.Context, m skills.Manager, sess *sessions.Session) ([]*skills.Skill, error) {
	if m == nil {
		return nil, nil
	}
	names := sess.ActiveSkills()
	out := make([]*skills.Skill, 0, len(names))
	for _, name := range names {
		sk, err := m.Load(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("load %q: %w", name, err)
		}
		out = append(out, sk)
	}
	return out, nil
}
