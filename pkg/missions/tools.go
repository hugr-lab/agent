package missions

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"

	"github.com/hugr-lab/hugen/pkg/missions/graph"
	"github.com/hugr-lab/hugen/pkg/sessions"
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
