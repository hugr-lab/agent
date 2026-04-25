package missions

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"

	"github.com/hugr-lab/hugen/pkg/missions/executor"
	"github.com/hugr-lab/hugen/pkg/missions/graph"
	"github.com/hugr-lab/hugen/pkg/sessions"
	"github.com/hugr-lab/hugen/pkg/skills"
	"github.com/hugr-lab/hugen/pkg/tools"
)

// SpawnServiceName is the tools.Manager provider key for the
// sub-agent spawn surface. The skills/_subagent autoload skill
// declares it via `provider: _mission_spawn` so the tool ships
// onto every sub-agent session — the per-role can_spawn /
// max_depth gates fire at tool-run time, not at bind time.
const SpawnServiceName = "_mission_spawn"

// AgentMaxSpawnDepth is the hard ceiling on parent-chain depth
// when a role's max_depth is 0 (= "use the agent-wide cap"). Kept
// in the package — operators tune via missions.MaxSpawnDepthAgent
// in config which the runtime threads through to NewSpawnService.
const AgentMaxSpawnDepth = 4

// SpawnService is the tools.Provider that exposes spawn_sub_mission
// to sub-agent sessions. One instance per runtime.
type SpawnService struct {
	executor *executor.Executor
	sessions *sessions.Manager
	skills   skills.Manager
	maxDepth int

	tools []tool.Tool
}

// SpawnConfig bundles the SpawnService's construction deps.
type SpawnConfig struct {
	Executor *executor.Executor
	Sessions *sessions.Manager
	Skills   skills.Manager
	// MaxSpawnDepth is the agent-wide depth cap applied when the
	// caller role's MaxDepth is 0. Zero falls back to
	// AgentMaxSpawnDepth (4).
	MaxSpawnDepth int
}

// NewSpawnService builds the spawn provider. Implements tools.Provider.
func NewSpawnService(cfg SpawnConfig) *SpawnService {
	depth := cfg.MaxSpawnDepth
	if depth <= 0 {
		depth = AgentMaxSpawnDepth
	}
	svc := &SpawnService{
		executor: cfg.Executor,
		sessions: cfg.Sessions,
		skills:   cfg.Skills,
		maxDepth: depth,
	}
	svc.tools = []tool.Tool{&spawnSubMissionTool{svc: svc}}
	return svc
}

func (s *SpawnService) Name() string         { return SpawnServiceName }
func (s *SpawnService) Tools() []tool.Tool   { return s.tools }

// ------------------------------------------------------------------
// spawn_sub_mission
// ------------------------------------------------------------------

type spawnSubMissionTool struct {
	svc *SpawnService
}

func (t *spawnSubMissionTool) Name() string { return "spawn_sub_mission" }
func (t *spawnSubMissionTool) Description() string {
	return "Queues a new mission as a peer in the coordinator's mission graph. Use only when " +
		"your task naturally decomposes into a separate specialist's work and your role's " +
		"frontmatter authorises spawning (`can_spawn: true`). Refusal envelopes (role lacks " +
		"can_spawn / depth cap exceeded / unknown skill,role / cross-graph dependency) do NOT " +
		"fail your own mission — keep working on what you can do directly."
}
func (t *spawnSubMissionTool) IsLongRunning() bool { return false }

func (t *spawnSubMissionTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: "OBJECT",
			Properties: map[string]*genai.Schema{
				"skill": {Type: "STRING", Description: "Loaded skill name."},
				"role":  {Type: "STRING", Description: "Specialist role declared under sub_agents in the skill's frontmatter."},
				"task":  {Type: "STRING", Description: "Self-contained task description for the spawned mission. ≤ 2000 chars."},
				"depends_on": {
					Type:        "ARRAY",
					Items:       &genai.Schema{Type: "STRING"},
					Description: "Optional list of mission_ids in this coordinator's graph that must reach `done` before the new mission starts.",
				},
			},
			Required: []string{"skill", "role", "task"},
		},
	}
}

func (t *spawnSubMissionTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *spawnSubMissionTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	return t.svc.Spawn(ctx, ctx.SessionID(), args), nil
}

// Spawn is the tool's behaviour exposed for direct testing. Returns
// the envelope (success or error). Always returns a non-nil map; nil
// error signals "envelope is the answer" so the LLM consumes it
// verbatim.
func (s *SpawnService) Spawn(ctx context.Context, callerSessionID string, args any) map[string]any {
	m, ok := args.(map[string]any)
	if !ok {
		return errorEnvelope("spawn_sub_mission: unexpected args")
	}
	skill, _ := m["skill"].(string)
	role, _ := m["role"].(string)
	task, _ := m["task"].(string)
	if strings.TrimSpace(skill) == "" || strings.TrimSpace(role) == "" || strings.TrimSpace(task) == "" {
		return errorEnvelope("spawn_sub_mission: skill, role, task are required")
	}
	dependsOn := decodeDeps(m["depends_on"])

	if s.sessions == nil {
		return errorEnvelope("spawn_sub_mission: session manager unavailable")
	}
	if callerSessionID == "" {
		return errorEnvelope("spawn_sub_mission: caller session id is empty")
	}
	caller, err := s.sessions.Session(callerSessionID)
	if err != nil {
		return errorEnvelope("spawn_sub_mission: resolve caller: " + err.Error())
	}
	callerSkill, callerRole, ok := callerRoleMeta(caller)
	if !ok {
		return errorEnvelope("spawn_sub_mission is only available on sub-agent sessions")
	}

	callerSpec, err := s.lookupRole(ctx, callerSkill, callerRole)
	if err != nil {
		return errorEnvelope(err.Error())
	}
	if !callerSpec.CanSpawn {
		return errorEnvelope(fmt.Sprintf("role %s/%s is not authorised to spawn sub-missions",
			callerSkill, callerRole))
	}

	if _, err := s.lookupRole(ctx, skill, role); err != nil {
		return errorEnvelope(fmt.Sprintf("unknown (skill, role) pair %s/%s", skill, role))
	}

	depth, coordID, err := s.walkDepth(caller)
	if err != nil {
		return errorEnvelope(err.Error())
	}
	cap := callerSpec.MaxDepth
	if cap <= 0 {
		cap = s.maxDepth
	}
	if depth+1 > cap {
		return errorEnvelope(fmt.Sprintf("spawn depth limit reached (max %d)", cap))
	}

	missionID, err := s.executor.RegisterSingle(coordID, skill, role, task, dependsOn)
	if err != nil {
		return errorEnvelope("spawn_sub_mission: " + err.Error())
	}
	return map[string]any{
		"mission_id": missionID,
		"status":     graph.StatusPending,
		"hint": "Your current mission continues. Call mission_status (from the coordinator) " +
			"for the new mission's state — the scheduler promotes it on the next tick.",
	}
}

// callerRoleMeta extracts (skill, role) cached on a sub-agent
// session at Create time. Returns ok=false when the caller is not
// a sub-agent (e.g. coordinator session, which carries empty
// skill/role metadata).
func callerRoleMeta(sess *sessions.Session) (skill, role string, ok bool) {
	skill = sess.Skill()
	role = sess.Role()
	if skill == "" || role == "" {
		return "", "", false
	}
	return skill, role, true
}

// lookupRole resolves (skill, role) → SubAgentSpec from the loaded
// skill manifest. Loads the skill if not already loaded.
func (s *SpawnService) lookupRole(ctx context.Context, skill, role string) (skills.SubAgentSpec, error) {
	if s.skills == nil {
		return skills.SubAgentSpec{}, fmt.Errorf("skills manager unavailable")
	}
	sk, err := s.skills.Load(ctx, skill)
	if err != nil {
		return skills.SubAgentSpec{}, fmt.Errorf("load skill %s: %w", skill, err)
	}
	spec, ok := sk.SubAgents[role]
	if !ok {
		return skills.SubAgentSpec{}, fmt.Errorf("unknown (skill, role) pair %s/%s", skill, role)
	}
	return spec, nil
}

// walkDepth counts hops from `caller` up the parent_session_id chain
// to the root coordinator. Returns the depth + the root id (= coord
// session id). Bounded by the agent-wide cap so a malformed chain
// can't infinite-loop.
func (s *SpawnService) walkDepth(caller *sessions.Session) (int, string, error) {
	depth := 0
	current := caller
	for current != nil {
		parentID := current.ParentSessionID()
		if parentID == "" {
			// Reached the root.
			return depth, current.ID(), nil
		}
		depth++
		if depth > s.maxDepth*4 {
			// Safety valve: walk shouldn't exceed hard agent cap × 4.
			return 0, "", fmt.Errorf("session chain depth exceeds hard limit (possible cycle)")
		}
		next, err := s.sessions.Session(parentID)
		if err != nil {
			return 0, "", fmt.Errorf("walk parent chain: %w", err)
		}
		current = next
	}
	return 0, "", fmt.Errorf("walk parent chain: caller has no root ancestor")
}

// decodeDeps coerces the LLM-supplied depends_on field (any) into a
// []string. Accepts []any, []string, or nil.
func decodeDeps(raw any) []string {
	switch v := raw.(type) {
	case nil:
		return nil
	case []string:
		out := make([]string, 0, len(v))
		for _, s := range v {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				if s = strings.TrimSpace(s); s != "" {
					out = append(out, s)
				}
			}
		}
		return out
	default:
		return nil
	}
}
