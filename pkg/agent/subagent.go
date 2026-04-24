package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	adksession "google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"

	"github.com/hugr-lab/hugen/pkg/models"
	"github.com/hugr-lab/hugen/pkg/sessions"
	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
	"github.com/hugr-lab/hugen/pkg/skills"
	"github.com/hugr-lab/hugen/pkg/tools"
)

// SubAgentProviderName is the tools.Manager provider key the
// _system skill references via `providers: [{provider: _subagent}]`.
// Registered in cmd/agent/runtime.go alongside the other autoload
// providers (_skills, _memory, _context).
const SubAgentProviderName = "_subagent"

// Dispatcher (spec 006 §4) opens a child session for a sub-agent
// dispatch, builds a transient llmagent wired to the cheap-model
// router intent, drives it through ADK Runner up to the role's turn
// cap, and returns a capped summary back to the coordinator.
//
// One Dispatcher is shared across the runtime; each Run is independent
// and creates its own child session + transient agent. Safe for
// concurrent calls from different coordinator sessions.
type Dispatcher struct {
	sessions *sessions.Manager
	skills   skills.Manager
	router   *models.Router
	logger   *slog.Logger

	// Timeout bounds each Run end-to-end. Defaults to 5 minutes when
	// zero.
	Timeout time.Duration
}

// DispatcherConfig bundles Dispatcher dependencies.
type DispatcherConfig struct {
	Sessions *sessions.Manager
	Skills   skills.Manager
	Router   *models.Router
	Logger   *slog.Logger
	Timeout  time.Duration
}

// NewDispatcher constructs a Dispatcher. All three dependencies are
// required; Logger defaults to slog.Default; Timeout defaults to 5m.
func NewDispatcher(cfg DispatcherConfig) (*Dispatcher, error) {
	if cfg.Sessions == nil {
		return nil, errors.New("subagent: Sessions manager required")
	}
	if cfg.Skills == nil {
		return nil, errors.New("subagent: Skills manager required")
	}
	if cfg.Router == nil {
		return nil, errors.New("subagent: Router required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Minute
	}
	return &Dispatcher{
		sessions: cfg.Sessions,
		skills:   cfg.Skills,
		router:   cfg.Router,
		logger:   cfg.Logger,
		Timeout:  cfg.Timeout,
	}, nil
}

// DispatchResult is the structured outcome of a sub-agent dispatch.
// Populated on both success and failure paths — Run never panics or
// returns a hard error to the LLM. Mapped 1:1 to the JSON tool result
// the coordinator sees.
type DispatchResult struct {
	// Summary is the specialist's final assistant text, capped at
	// SubAgentSpec.SummaryMaxTok runes. Empty on hard failure.
	Summary string `json:"summary"`
	// ChildSessionID is the hub.db.sessions row created for this
	// dispatch (session_type = "subagent").
	ChildSessionID string `json:"child_session"`
	// TurnsUsed counts assistant-side turns the specialist consumed
	// (model invocations, not raw events).
	TurnsUsed int `json:"turns_used"`
	// Truncated is true when Summary was clipped to fit
	// SummaryMaxTok.
	Truncated bool `json:"truncated"`
	// Error is the failure reason; empty string on success.
	// Populated for: turn-cap reached, tool-or-model errors,
	// pre-flight refusals.
	Error string `json:"error,omitempty"`
}

// Run dispatches a sub-agent. Steps (spec 006 §4):
//
//  1. Resolve parent skill + role; pre-flight checks.
//  2. Open a child session via SessionManager.Create with the five
//     well-known State keys (session_type=subagent, parent linkage,
//     spawn-event id, mission task, fork_after_seq=nil).
//  3. Apply the parent skill to the child via Session.LoadSkill (the
//     child autoload set already includes _memory / _context per
//     `autoload_for: [root, subagent]`, so the specialist has memory
//     scratchpad + context-status tools out of the box).
//  4. Construct a transient llmagent wired to Router.ModelFor(intent).
//     The InstructionProvider fronts the role's instructions, then
//     delegates to the child session's Snapshot for the rest of the
//     prompt (skill body + autoload bodies + memory hint + notes).
//  5. Drive Runner.Run with `task` (and optional `notes`) as the
//     first user message. Iterate events, count turns, capture the
//     terminal assistant text.
//  6. Cap final text to SummaryMaxTok runes; mark child session
//     completed/failed/abandoned in hub; return DispatchResult.
//
// `parentSessionID` is the coordinator's session id. `spawnEventID`
// is the id of the tool_call event on the parent that triggered the
// dispatch (empty in tests; production wiring sets it).
func (d *Dispatcher) Run(
	ctx context.Context,
	parentSessionID, spawnEventID string,
	parentSkill, role string,
	spec skills.SubAgentSpec,
	task, notes string,
) (DispatchResult, error) {
	if strings.TrimSpace(task) == "" {
		return DispatchResult{Error: "task is empty"}, nil
	}
	if spec.MaxTurns <= 0 {
		spec.MaxTurns = defaultDispatchMaxTurns
	}
	if spec.SummaryMaxTok <= 0 {
		spec.SummaryMaxTok = defaultDispatchSummaryMaxTok
	}

	// Pre-flight (spec 006 §4 step 3): refuse oversized tasks before
	// opening any state, so the coordinator gets a fast structured
	// error instead of a half-spawned mission.
	intent := models.Intent(spec.Intent)
	budget := d.router.BudgetFor(intent)
	if pretokens := approxTokenCount(task) + approxTokenCount(notes); budget > 0 && pretokens > budget/2 {
		return DispatchResult{
			Error: fmt.Sprintf("task would exceed cheap-model budget (estimated %d tokens vs budget %d)", pretokens, budget),
		}, nil
	}

	// Bound the whole dispatch end-to-end so a runaway loop / stuck
	// model can't hang the coordinator.
	runCtx, cancel := context.WithTimeout(ctx, d.Timeout)
	defer cancel()

	// Step 2 — open child session via Manager.Create with the five
	// well-known State keys. Manager.Create handles linkage + hub
	// row + autoload (filtered to subagent-applicable skills).
	childID := newDispatchSessionID()
	createReq := &adksession.CreateRequest{
		AppName:   sessionAppName(d.sessions, parentSessionID),
		UserID:    sessionUserID(d.sessions, parentSessionID),
		SessionID: childID,
		State: map[string]any{
			"__session_type__":          sessstore.SessionTypeSubAgent,
			"__parent_session_id__":     parentSessionID,
			"__spawned_from_event_id__": spawnEventID,
			"__mission__":               task,
			"__skill__":                 parentSkill,
			"__role__":                  role,
			// __fork_after_seq__ omitted — sub-agents always have own context.
		},
	}
	if _, err := d.sessions.Create(runCtx, createReq); err != nil {
		return DispatchResult{
			ChildSessionID: childID,
			Error:          fmt.Sprintf("open child session: %v", err),
		}, nil
	}

	childSess, err := d.sessions.Session(childID)
	if err != nil {
		return DispatchResult{
			ChildSessionID: childID,
			Error:          fmt.Sprintf("resolve child session: %v", err),
		}, nil
	}

	// Step 3 — apply parent skill to the child. Allowlist semantics
	// from spec.Tools / spec.ToolAllowlist filter the binding set
	// down once Phase-3 tooling-render lands; for now we load the
	// full parent skill (the example role's allowlist matches the
	// only provider on the demo skill, so the user-facing behaviour
	// is identical pending the follow-up filter pass).
	if err := childSess.LoadSkill(runCtx, parentSkill); err != nil {
		d.markChild(runCtx, childID, "failed")
		return DispatchResult{
			ChildSessionID: childID,
			Error:          fmt.Sprintf("load parent skill %q: %v", parentSkill, err),
		}, nil
	}

	// Step 4 — build the transient llmagent. The InstructionProvider
	// fronts the role's instructions, then delegates to the child
	// session's Snapshot prompt so memory hint / autoload skill
	// bodies / session notes all flow through the same machinery
	// the coordinator uses.
	roleInstr := strings.TrimSpace(spec.Instructions)
	instr := func(ctx agent.ReadonlyContext) (string, error) {
		base, err := BaseInstructionProvider(d.sessions)(ctx)
		if err != nil {
			return "", err
		}
		if roleInstr == "" {
			return base, nil
		}
		if base == "" {
			return roleInstr, nil
		}
		return roleInstr + "\n\n" + base, nil
	}

	subAgent, err := llmagent.New(llmagent.Config{
		Name:                fmt.Sprintf("subagent_%s_%s", parentSkill, role),
		Description:         spec.Description,
		Model:               d.router.ModelFor(intent),
		Toolsets:            nil, // sessions.Inject is the single source of truth
		InstructionProvider: instr,
		BeforeModelCallbacks: []llmagent.BeforeModelCallback{
			sessions.Inject(d.sessions),
		},
	})
	if err != nil {
		d.markChild(runCtx, childID, "failed")
		return DispatchResult{
			ChildSessionID: childID,
			Error:          fmt.Sprintf("build sub-agent llmagent: %v", err),
		}, nil
	}

	r, err := runner.New(runner.Config{
		AppName:        createReq.AppName,
		Agent:          subAgent,
		SessionService: d.sessions,
	})
	if err != nil {
		d.markChild(runCtx, childID, "failed")
		return DispatchResult{
			ChildSessionID: childID,
			Error:          fmt.Sprintf("build sub-agent runner: %v", err),
		}, nil
	}

	// Step 5 — drive turns.
	userMsg := buildUserMessage(task, notes)
	result := DispatchResult{ChildSessionID: childID}

	// We count "turns" as terminal model events (TurnComplete=true on
	// the agent role). Tool calls show up as separate events and are
	// not counted as turns by the coordinator's lens.
	for ev, runErr := range r.Run(runCtx, createReq.UserID, childID, userMsg, agent.RunConfig{}) {
		if runErr != nil {
			d.markChild(runCtx, childID, "failed")
			result.Error = fmt.Sprintf("model error: %v", runErr)
			return result, nil
		}
		if ev == nil {
			continue
		}
		// Capture the most recent agent text as candidate summary.
		if text, finalTurn := agentTurnText(ev); finalTurn {
			result.TurnsUsed++
			if text != "" {
				result.Summary = text
			}
			if result.TurnsUsed >= spec.MaxTurns {
				// Hit the cap — abandoned.
				if !hasFunctionCall(ev) {
					// Last turn produced text without further tool calls,
					// so it's a clean termination right at the cap.
					break
				}
				// Otherwise the specialist is still asking for tools and
				// we won't let it run forever.
				d.markChild(runCtx, childID, "abandoned")
				if result.Summary == "" {
					result.Summary = ""
				}
				result.Error = fmt.Sprintf("turn cap reached after %d turns", spec.MaxTurns)
				return cap(result, spec.SummaryMaxTok), nil
			}
		}
	}

	if runCtx.Err() != nil && result.Error == "" {
		d.markChild(runCtx, childID, "abandoned")
		result.Error = fmt.Sprintf("dispatch cancelled: %v", runCtx.Err())
		return cap(result, spec.SummaryMaxTok), nil
	}

	d.markChild(runCtx, childID, "completed")
	return cap(result, spec.SummaryMaxTok), nil
}

// ToolFor returns an ADK tool.Tool exposing this dispatcher to the
// coordinator's LLM under the canonical name
// `subagent_<skill>_<role>`. Phase-3 follow-up wires the registration
// into the skill load path; tests construct it directly.
//
// Parked behind a TODO until Phase-3 T103 ships the tool.Tool wrapper.
// The Run method above carries the full dispatch contract today.

// markChild updates the child session's status. Best-effort — a hub
// write failure here doesn't bubble up to the coordinator; the row's
// status drift is rare and self-correcting at the next reviewer pass.
// No-op when the SessionManager runs without a hub (test mode).
func (d *Dispatcher) markChild(ctx context.Context, childID, status string) {
	if err := d.sessions.UpdateSessionStatus(ctx, childID, status); err != nil {
		d.logger.Warn("subagent: update child status",
			"child", childID, "status", status, "err", err)
	}
}

// ----- helpers -----

const (
	defaultDispatchMaxTurns      = 15
	defaultDispatchSummaryMaxTok = 800
)

// approxTokenCount is a coarse heuristic mirroring
// pkg/models.TokenEstimator's default ratio (≈4 chars per token).
// Used only for the pre-flight refusal check; precise tokenisation
// isn't worth the dep.
func approxTokenCount(s string) int {
	return (len(s) + 3) / 4
}

// newDispatchSessionID generates a session id for a sub-agent run.
// Format: `sub_<uuid>` so it's distinguishable from coordinator
// sessions (which use raw UUIDs) at a glance in logs/db.
func newDispatchSessionID() string {
	return "sub_" + uuid.NewString()
}

// sessionAppName / sessionUserID best-effort look up the parent's
// app / user for the child session. Falls back to defaults when the
// parent is unknown — the child still works, it just inherits less
// context.
func sessionAppName(sm *sessions.Manager, parentID string) string {
	if sm == nil || parentID == "" {
		return "hugr-agent"
	}
	if sess, err := sm.Session(parentID); err == nil && sess != nil {
		if app := sess.AppName(); app != "" {
			return app
		}
	}
	return "hugr-agent"
}

func sessionUserID(sm *sessions.Manager, parentID string) string {
	if sm == nil || parentID == "" {
		return "subagent"
	}
	if sess, err := sm.Session(parentID); err == nil && sess != nil {
		if u := sess.UserID(); u != "" {
			return u
		}
	}
	return "subagent"
}

// buildUserMessage packs task + optional notes into the first
// user_message. Notes appear under a labelled section so the
// specialist's LLM can distinguish "extra context" from the
// requested task.
func buildUserMessage(task, notes string) *genai.Content {
	body := strings.TrimSpace(task)
	if n := strings.TrimSpace(notes); n != "" {
		body = body + "\n\n## Notes\n" + n
	}
	return &genai.Content{
		Role:  "user",
		Parts: []*genai.Part{{Text: body}},
	}
}

// agentTurnText returns (text, finalTurn) for an event. finalTurn is
// true when the event represents a completed model turn from an
// agent role; text is the concatenated text parts (empty if the turn
// only carried tool calls). Function calls / tool responses are NOT
// final turns from the dispatcher's perspective.
func agentTurnText(ev *adksession.Event) (string, bool) {
	if ev == nil || ev.Content == nil {
		return "", false
	}
	if ev.Content.Role != "model" && ev.Content.Role != "agent" {
		return "", false
	}
	if !ev.TurnComplete {
		return "", false
	}
	var b strings.Builder
	for _, p := range ev.Content.Parts {
		if p == nil || p.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(p.Text)
	}
	return b.String(), true
}

// hasFunctionCall reports whether any part of the event content is a
// FunctionCall. Used at the turn cap to distinguish "agent finished
// cleanly at the cap" from "agent still wants tools".
func hasFunctionCall(ev *adksession.Event) bool {
	if ev == nil || ev.Content == nil {
		return false
	}
	for _, p := range ev.Content.Parts {
		if p != nil && p.FunctionCall != nil {
			return true
		}
	}
	return false
}

// cap truncates result.Summary to maxRunes, setting Truncated when it
// fires. Operates on runes (not bytes) so multibyte content isn't
// chopped mid-rune.
func cap(result DispatchResult, maxRunes int) DispatchResult {
	if maxRunes <= 0 {
		return result
	}
	r := []rune(result.Summary)
	if len(r) <= maxRunes {
		return result
	}
	result.Summary = string(r[:maxRunes])
	result.Truncated = true
	return result
}

// MarshalDispatchResult is a convenience for tool implementations to
// flatten DispatchResult into a JSON string. Returns an empty
// string + an error only on programming bugs (the struct has no
// unmarshalable fields), so callers can ignore the error in the
// happy path.
func MarshalDispatchResult(r DispatchResult) (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("subagent: marshal result: %w", err)
	}
	return string(b), nil
}

// ============================================================================
// Tools provider — exposes subagent_dispatch + subagent_list to the
// coordinator LLM (spec 006 phase 1 §T103). Registered under the
// provider name SubAgentProviderName and referenced by the _system
// skill's frontmatter so the tools ride along with the rest of the
// skill-management surface (skill_load / skill_list / ...).
// ============================================================================

// SubAgentService is the tools.Provider that exposes the sub-agent
// dispatch tools to the coordinator LLM.
//
// Two tools ship:
//
//   - subagent_list() — returns the specialist roles declared by
//     every currently-loaded skill on the calling session, so the
//     LLM can discover what's available before deciding to delegate.
//   - subagent_dispatch(skill, role, task, notes?) — runs the
//     specialist through the Dispatcher and returns DispatchResult.
//
// The service itself carries no state — it's a thin wrapper around
// a shared Dispatcher. One instance per runtime.
type SubAgentService struct {
	dispatcher *Dispatcher
	sessions   *sessions.Manager
	skills     skills.Manager
	tools      []tool.Tool
}

// NewSubAgentService constructs the provider.
func NewSubAgentService(d *Dispatcher, sm *sessions.Manager, sk skills.Manager) (*SubAgentService, error) {
	if d == nil {
		return nil, errors.New("subagent: service requires Dispatcher")
	}
	if sm == nil {
		return nil, errors.New("subagent: service requires SessionManager")
	}
	if sk == nil {
		return nil, errors.New("subagent: service requires skills.Manager")
	}
	s := &SubAgentService{dispatcher: d, sessions: sm, skills: sk}
	s.tools = []tool.Tool{
		&subagentListTool{sm: sm, skills: sk},
		&subagentDispatchTool{dispatcher: d, sm: sm, skills: sk},
	}
	return s, nil
}

// Name implements tools.Provider.
func (s *SubAgentService) Name() string { return SubAgentProviderName }

// Tools implements tools.Provider.
func (s *SubAgentService) Tools() []tool.Tool { return s.tools }

// sessionForTool is the tool-context session resolver. Duplicated
// here rather than importing chatcontext's sessionFor to keep
// pkg/agent free of an additional package dep.
func sessionForTool(ctx tool.Context, sm *sessions.Manager) (*sessions.Session, error) {
	sid := ctx.SessionID()
	if sid == "" {
		return nil, fmt.Errorf("no session id in tool context")
	}
	return sm.Session(sid)
}

// ----- subagent_list -----

type subagentListTool struct {
	sm     *sessions.Manager
	skills skills.Manager
}

func (t *subagentListTool) Name() string { return "subagent_list" }
func (t *subagentListTool) Description() string {
	return "Lists the specialist sub-agent roles declared by every currently-loaded skill on this session. Returns {available: [{skill, role, description, intent}]}. Call before subagent_dispatch when unsure which role fits the task."
}
func (t *subagentListTool) IsLongRunning() bool { return false }

func (t *subagentListTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
	}
}

func (t *subagentListTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *subagentListTool) Run(ctx tool.Context, _ any) (map[string]any, error) {
	sess, err := sessionForTool(ctx, t.sm)
	if err != nil {
		return nil, fmt.Errorf("subagent_list: %w", err)
	}

	type entry struct {
		Skill       string `json:"skill"`
		Role        string `json:"role"`
		Description string `json:"description"`
		Intent      string `json:"intent,omitempty"`
	}
	var out []entry
	for _, skillName := range sess.ActiveSkills() {
		sk, err := t.skills.Load(ctx, skillName)
		if err != nil || sk == nil {
			continue
		}
		for role, spec := range sk.SubAgents {
			out = append(out, entry{
				Skill:       sk.Name,
				Role:        role,
				Description: spec.Description,
				Intent:      spec.Intent,
			})
		}
	}
	// Stable order: (skill, role) lexicographic.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Skill != out[j].Skill {
			return out[i].Skill < out[j].Skill
		}
		return out[i].Role < out[j].Role
	})
	return map[string]any{"available": out}, nil
}

// ----- subagent_dispatch -----

type subagentDispatchTool struct {
	dispatcher *Dispatcher
	sm         *sessions.Manager
	skills     skills.Manager
}

func (t *subagentDispatchTool) Name() string { return "subagent_dispatch" }
func (t *subagentDispatchTool) Description() string {
	return "Delegates a narrow task to a specialist sub-agent running in an isolated context. Returns {summary, child_session, turns_used, truncated, error}. The specialist runs under the role's configured model (often a cheaper one) and writes its transcript to its own session — the coordinator's prompt only sees the capped summary. Call subagent_list first to discover available roles."
}
func (t *subagentDispatchTool) IsLongRunning() bool { return true }

func (t *subagentDispatchTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: "OBJECT",
			Properties: map[string]*genai.Schema{
				"skill": {
					Type:        "STRING",
					Description: "Name of a loaded skill that declares sub_agents. Must be currently active on this session — call skill_load first if needed.",
				},
				"role": {
					Type:        "STRING",
					Description: "Specialist role declared under sub_agents in the skill's frontmatter (e.g. \"schema_explorer\"). Use subagent_list to discover.",
				},
				"task": {
					Type:        "STRING",
					Description: "Natural-language task description for the specialist. Keep it focused — a specialist runs one mission.",
				},
				"notes": {
					Type:        "STRING",
					Description: "Optional extra context or constraints for the specialist (e.g. data filters, timeframe).",
				},
			},
			Required: []string{"skill", "role", "task"},
		},
	}
}

func (t *subagentDispatchTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *subagentDispatchTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	m, ok := args.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("subagent_dispatch: unexpected args: %T", args)
	}
	skillName, _ := m["skill"].(string)
	role, _ := m["role"].(string)
	task, _ := m["task"].(string)
	notes, _ := m["notes"].(string)

	if strings.TrimSpace(skillName) == "" || strings.TrimSpace(role) == "" || strings.TrimSpace(task) == "" {
		return nil, fmt.Errorf("subagent_dispatch: skill, role, and task are required")
	}

	parent, err := sessionForTool(ctx, t.sm)
	if err != nil {
		return nil, fmt.Errorf("subagent_dispatch: %w", err)
	}

	// Guard: the named skill must be active on this session. Prevents
	// a coordinator from dispatching into a skill it hasn't loaded
	// (which would silently expose a different toolset to the
	// specialist than the user expects).
	if !parent.HasSkill(skillName) {
		return nil, fmt.Errorf("subagent_dispatch: skill %q is not loaded on this session; call skill_load first", skillName)
	}

	sk, err := t.skills.Load(ctx, skillName)
	if err != nil {
		return nil, fmt.Errorf("subagent_dispatch: load skill %q: %w", skillName, err)
	}
	spec, ok := sk.SubAgents[role]
	if !ok {
		return nil, fmt.Errorf("subagent_dispatch: skill %q has no sub_agent role %q (call subagent_list to see available roles)", skillName, role)
	}

	// spawnEventID linkage to the coordinator's dispatching tool_call
	// event is recorded via spec 006 migration 0.0.2
	// (sessions.spawned_from_event_id). ADK's tool.Context doesn't
	// expose the invoking function-call id today, so we leave it
	// empty here — the child's parent_session_id + metadata already
	// record enough linkage for Phase 1. A post-Phase-2 follow-up can
	// read the id from the session's last tool_call event (single SQL
	// query at dispatch time) once the classifier is writing it.
	res, runErr := t.dispatcher.Run(
		ctx,
		parent.ID(), "",
		skillName, role,
		spec,
		task, notes,
	)
	if runErr != nil {
		// Shouldn't happen — Dispatcher.Run funnels everything through
		// DispatchResult. Surface as a tool error so the LLM sees it.
		return nil, fmt.Errorf("subagent_dispatch: %w", runErr)
	}

	return map[string]any{
		"summary":       res.Summary,
		"child_session": res.ChildSessionID,
		"turns_used":    res.TurnsUsed,
		"truncated":     res.Truncated,
		"error":         res.Error,
	}, nil
}
