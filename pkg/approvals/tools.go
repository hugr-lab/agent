package approvals

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"

	"github.com/hugr-lab/hugen/pkg/tools"
)

// All tool struct types in this file are unexported. They hold a
// *Manager back-reference and dispatch to the manager's domain
// methods. The manager itself is the tools.Provider — no separate
// adapter layer.
//
// US1 ships approval_respond. US2 adds policy_list / policy_set /
// policy_remove. US3 adds ask_coordinator.

// ─────────────────────────────────────────────────────────────────
// approval_respond  (coordinator-only)
// ─────────────────────────────────────────────────────────────────

type approvalRespondTool struct {
	m *Manager
}

func (t *approvalRespondTool) Name() string { return "approval_respond" }

func (t *approvalRespondTool) Description() string {
	return "Resolve a pending HITL approval. Translate the user's free-form reply (\"approve app-xxx\", \"reject app-xxx because ...\", \"modify app-xxx {<json>}\", \"answer app-xxx <text>\") into this tool call. Decision: approve | reject | modify | answer. modified_args is REQUIRED when decision=modify; answer is REQUIRED when decision=answer (only valid for ask_coordinator approvals)."
}

func (t *approvalRespondTool) IsLongRunning() bool { return false }

func (t *approvalRespondTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: "OBJECT",
			Properties: map[string]*genai.Schema{
				"id": {
					Type:        "STRING",
					Description: "Approval id (e.g. app-7c9d).",
				},
				"decision": {
					Type:        "STRING",
					Description: "User's decision: approve | reject | modify | answer.",
					Enum:        []string{"approve", "reject", "modify", "answer"},
				},
				"modified_args": {
					Type:        "OBJECT",
					Description: "Required when decision=modify. JSON object replacing the original tool call's args.",
				},
				"answer": {
					Type:        "STRING",
					Description: "Required when decision=answer. Free-form text the sub-agent will see as the ask_coordinator tool's result.",
				},
				"note": {
					Type:        "STRING",
					Description: "Optional — rejection reason or trailing approval note. Captured on the audit record.",
				},
			},
			Required: []string{"id", "decision"},
		},
	}
}

func (t *approvalRespondTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *approvalRespondTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	m, ok := args.(map[string]any)
	if !ok {
		return errEnvelope("approval_respond", fmt.Errorf("unexpected args type %T", args), "invalid_args")
	}

	id := stringArg(m, "id")
	decision := Decision(stringArg(m, "decision"))
	note := stringArg(m, "note")
	answer := stringArg(m, "answer")

	var modifiedArgs map[string]any
	if v, ok := m["modified_args"].(map[string]any); ok {
		modifiedArgs = v
	}

	ref, err := t.m.Respond(ctx, RespondPayload{
		ApprovalID:   id,
		Decision:     decision,
		ModifiedArgs: modifiedArgs,
		Note:         note,
		Answer:       answer,
		ResponderID:  "user", // coordinator always relays user replies
	})
	if err != nil {
		return errEnvelope("approval_respond", err, classifyError(err))
	}

	return map[string]any{
		"ok":           true,
		"id":           ref.ID,
		"status":       string(ref.Status),
		"responded_at": "now",
	}, nil
}

// ─────────────────────────────────────────────────────────────────
// pending_approvals  (coordinator-only)
// ─────────────────────────────────────────────────────────────────

// pendingApprovalsTool is the coord-side discovery surface for
// open approval rows. The runtime DOES NOT auto-inject pending
// approvals into the coord prompt — the coord LLM is expected to
// call this tool when the user references an approval (e.g. "approve
// the cleanup"), so it can resolve the canonical app-id without
// scanning the event history. Recursion-guarded so it never gates
// itself (see gate.go::selfAuthenticatingTools).
type pendingApprovalsTool struct {
	m *Manager
}

func (t *pendingApprovalsTool) Name() string { return "pending_approvals" }

func (t *pendingApprovalsTool) Description() string {
	return "List pending HITL approval rows surfaced on YOUR coordinator session. Returns each row's id (app-...), tool_name, risk, mission_id, age, args excerpt, hitl_kind (approval | ask), and the legal reply choices. Call this whenever the user references an approval ('approve the cleanup', 'reject it') so you can resolve the canonical id before invoking approval_respond. Returns [] when nothing is pending."
}

func (t *pendingApprovalsTool) IsLongRunning() bool { return false }

func (t *pendingApprovalsTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: "OBJECT",
			Properties: map[string]*genai.Schema{
				"limit": {
					Type:        "INTEGER",
					Description: "Optional max rows to return. Defaults to 20, capped at 200.",
				},
			},
		},
	}
}

func (t *pendingApprovalsTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *pendingApprovalsTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	coordSessionID := ctx.SessionID()
	if coordSessionID == "" {
		return errEnvelope("pending_approvals", fmt.Errorf("no session id in context"), "internal_error")
	}

	limit := 20
	if m, ok := args.(map[string]any); ok {
		if v, ok := m["limit"].(float64); ok && v > 0 {
			limit = int(v)
		}
		if v, ok := m["limit"].(int); ok && v > 0 {
			limit = v
		}
	}

	rows, err := t.m.ListPending(toolCtxAsContext(ctx), coordSessionID, limit)
	if err != nil {
		return errEnvelope("pending_approvals", err, "internal_error")
	}

	now := t.m.nowFn()
	pending := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		ageMin := int(now.Sub(row.CreatedAt).Minutes())
		hitlKind := string(HITLKindApproval)
		choices := []string{"approve", "reject", "modify"}
		if row.ToolName == "ask_coordinator" {
			hitlKind = string(HITLKindAsk)
			choices = []string{"answer"}
		}
		pending = append(pending, map[string]any{
			"id":            row.ID,
			"tool_name":     row.ToolName,
			"risk":          string(row.Risk),
			"hitl_kind":     hitlKind,
			"mission_id":    row.MissionSessionID,
			"created_at":    row.CreatedAt.UTC().Format(time.RFC3339),
			"age_minutes":   ageMin,
			"args_digest":   argsDigest(row.Args),
			"choices":       choices,
		})
	}

	return map[string]any{
		"ok":      true,
		"pending": pending,
		"count":   len(pending),
	}, nil
}

// toolCtxAsContext extracts a context.Context from the ADK
// tool.Context (which embeds it). Same helper logic as
// callback.go::contextFromToolCtx; duplicated here to avoid
// re-exporting an internal helper.
func toolCtxAsContext(toolCtx tool.Context) context.Context {
	if c, ok := any(toolCtx).(context.Context); ok {
		return c
	}
	return context.Background()
}

// ─────────────────────────────────────────────────────────────────
// policy_list / policy_set / policy_remove (coordinator-only)
// ─────────────────────────────────────────────────────────────────

type policyListTool struct {
	m *Manager
}

func (t *policyListTool) Name() string { return "policy_list" }

func (t *policyListTool) Description() string {
	return "List persistent tool policies from the hot cache. Returns each row's tool_name, scope, policy (always_allowed | manual_required | denied), note, created_by, and updated_at. Optional filters: scope (global | skill:<name> | role:<skill>:<role>), tool_name (exact). Use this BEFORE policy_set to avoid duplicates and to verify the chain you'd shadow."
}

func (t *policyListTool) IsLongRunning() bool { return false }

func (t *policyListTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: "OBJECT",
			Properties: map[string]*genai.Schema{
				"scope": {
					Type:        "STRING",
					Description: "Optional scope filter: global | skill:<name> | role:<skill>:<role>.",
				},
				"tool_name": {
					Type:        "STRING",
					Description: "Optional exact tool name filter (e.g. data-execute_mutation or data-* for the prefix glob row).",
				},
			},
		},
	}
}

func (t *policyListTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *policyListTool) Run(_ tool.Context, args any) (map[string]any, error) {
	var scope, toolName string
	if m, ok := args.(map[string]any); ok {
		scope = stringArg(m, "scope")
		toolName = stringArg(m, "tool_name")
	}
	rows := t.m.PolicyStore().List(scope, toolName)
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		entry := map[string]any{
			"tool_name":  r.ToolName,
			"scope":      r.Scope,
			"policy":     r.Decision.String(),
			"note":       r.Note,
			"created_by": r.CreatedBy,
		}
		if !r.UpdatedAt.IsZero() {
			entry["updated_at"] = r.UpdatedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, entry)
	}
	return map[string]any{
		"ok":       true,
		"policies": out,
		"count":    len(out),
	}, nil
}

type policySetTool struct {
	m *Manager
}

func (t *policySetTool) Name() string { return "policy_set" }

func (t *policySetTool) Description() string {
	return "Persist a tool-policy override. tool_name is exact (data-execute_mutation) or a prefix glob (data-*). policy ∈ {always_allowed, manual_required, denied}. scope ∈ {global, skill:<name>, role:<skill>:<role>}. The runtime enforces a safety net: setting policy=always_allowed on a tool currently resolving to manual_required itself triggers an approval (meta-approval) before taking effect — this defuses prompt-injection attempts to silence the gate. Idempotent on identical rows (no event emitted)."
}

func (t *policySetTool) IsLongRunning() bool { return false }

func (t *policySetTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: "OBJECT",
			Properties: map[string]*genai.Schema{
				"tool_name": {
					Type:        "STRING",
					Description: "Exact tool name OR prefix glob ending in * (e.g. data-*).",
				},
				"policy": {
					Type:        "STRING",
					Description: "Decision: always_allowed | manual_required | denied.",
					Enum:        []string{"always_allowed", "manual_required", "denied"},
				},
				"scope": {
					Type:        "STRING",
					Description: "global | skill:<name> | role:<skill>:<role>. Prefer the narrowest scope that fits the user's intent.",
				},
				"note": {
					Type:        "STRING",
					Description: "Free-form annotation captured for audit (recommended).",
				},
			},
			Required: []string{"tool_name", "policy", "scope"},
		},
	}
}

func (t *policySetTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *policySetTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	m, ok := args.(map[string]any)
	if !ok {
		return errEnvelope("policy_set", fmt.Errorf("unexpected args type %T", args), "invalid_args")
	}
	toolName := stringArg(m, "tool_name")
	policyStr := stringArg(m, "policy")
	scope := stringArg(m, "scope")
	note := stringArg(m, "note")

	decision, err := ParsePolicyDecision(policyStr)
	if err != nil {
		return errEnvelope("policy_set", err, "invalid_policy")
	}

	pol := Policy{
		AgentID:   t.m.AgentID(),
		ToolName:  toolName,
		Scope:     scope,
		Decision:  decision,
		Note:      note,
		CreatedBy: "llm",
	}

	stdCtx := toolCtxAsContext(ctx)
	old, changed, err := t.m.PolicyStore().Set(stdCtx, pol)
	if err != nil {
		return errEnvelope("policy_set", err, classifyPolicyError(err))
	}

	if changed {
		t.m.EmitPolicyChanged(stdCtx, ctx.SessionID(), PolicyChangedMeta{
			ToolName:  toolName,
			Scope:     scope,
			OldPolicy: nonEmptyOr(old, "<none>"),
			NewPolicy: decision.String(),
			Note:      note,
			CreatedBy: "llm",
		})
	}

	return map[string]any{
		"ok":         true,
		"changed":    changed,
		"tool_name":  toolName,
		"scope":      scope,
		"policy":     decision.String(),
		"old_policy": old,
	}, nil
}

type policyRemoveTool struct {
	m *Manager
}

func (t *policyRemoveTool) Name() string { return "policy_remove" }

func (t *policyRemoveTool) Description() string {
	return "Remove a persistent tool-policy row by exact (tool_name, scope) match. Returns existed=true when a row was deleted. Removing a manual_required row does NOT trigger the safe_policy_change safety net even if it effectively widens the resolved policy — to keep that protection, set the policy to manual_required at a broader scope rather than removing the narrower row."
}

func (t *policyRemoveTool) IsLongRunning() bool { return false }

func (t *policyRemoveTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: "OBJECT",
			Properties: map[string]*genai.Schema{
				"tool_name": {Type: "STRING", Description: "Exact tool name or prefix glob — must match the row exactly."},
				"scope":     {Type: "STRING", Description: "Exact scope — must match the row exactly."},
			},
			Required: []string{"tool_name", "scope"},
		},
	}
}

func (t *policyRemoveTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *policyRemoveTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	m, ok := args.(map[string]any)
	if !ok {
		return errEnvelope("policy_remove", fmt.Errorf("unexpected args type %T", args), "invalid_args")
	}
	toolName := stringArg(m, "tool_name")
	scope := stringArg(m, "scope")

	stdCtx := toolCtxAsContext(ctx)
	old, existed, err := t.m.PolicyStore().Remove(stdCtx, t.m.AgentID(), toolName, scope)
	if err != nil {
		return errEnvelope("policy_remove", err, classifyPolicyError(err))
	}

	if existed {
		t.m.EmitPolicyChanged(stdCtx, ctx.SessionID(), PolicyChangedMeta{
			ToolName:  toolName,
			Scope:     scope,
			OldPolicy: old,
			NewPolicy: "removed",
			CreatedBy: "llm",
		})
	}

	return map[string]any{
		"ok":         true,
		"existed":    existed,
		"tool_name":  toolName,
		"scope":      scope,
		"old_policy": old,
	}, nil
}

func classifyPolicyError(err error) string {
	switch {
	case errors.Is(err, ErrInvalidScope):
		return "invalid_scope"
	case errors.Is(err, ErrInvalidPolicy):
		return "invalid_policy"
	case errors.Is(err, ErrInvalidToolName):
		return "invalid_tool_name"
	default:
		return "internal_error"
	}
}

func nonEmptyOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// ─────────────────────────────────────────────────────────────────
// ask_coordinator  (sub-agent only)
// ─────────────────────────────────────────────────────────────────

// askCoordinatorTool lets a sub-agent escalate an ambiguous decision
// to the coordinator (and ultimately the user). Reuses the
// approvals plumbing with tool_name="ask_coordinator" and
// hitl_kind="ask"; the coordinator answers via approval_respond
// with decision="answer", and the answer flows back as the
// sub-agent's tool result on the next dispatch.
//
// Authorization: sub-agent only. Coordinator calls get
// ErrForbiddenForCoordinator (the coord asks the user directly,
// not itself).
type askCoordinatorTool struct {
	m *Manager
}

func (t *askCoordinatorTool) Name() string { return "ask_coordinator" }

func (t *askCoordinatorTool) Description() string {
	return "Escalate an ambiguous decision to the coordinator/user. Use this when you can't choose between options without input — e.g. multiple equally-likely data sources, an unclear user intent, or a destructive action whose target is ambiguous. The runtime pauses your mission and surfaces your question on the coordinator session with hitl_kind=ask. The coordinator (or user via the coordinator) answers; you receive the answer string as your tool result on the next dispatch and proceed."
}

func (t *askCoordinatorTool) IsLongRunning() bool { return false }

func (t *askCoordinatorTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: "OBJECT",
			Properties: map[string]*genai.Schema{
				"question": {
					Type:        "STRING",
					Description: "Your question to the coordinator/user. One sentence is best; be specific about what input you need.",
				},
				"suggested": {
					Type:        "ARRAY",
					Items:       &genai.Schema{Type: "STRING"},
					Description: "Optional suggested answers/choices. The coordinator's reply isn't constrained to these — they're hints to make answering easier.",
				},
			},
			Required: []string{"question"},
		},
	}
}

func (t *askCoordinatorTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *askCoordinatorTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	m, ok := args.(map[string]any)
	if !ok {
		return errEnvelope("ask_coordinator", fmt.Errorf("unexpected args type %T", args), "invalid_args")
	}
	question := stringArg(m, "question")
	if question == "" {
		return errEnvelope("ask_coordinator", fmt.Errorf("question required"), "invalid_args")
	}
	var suggested []string
	if v, ok := m["suggested"].([]any); ok {
		for _, s := range v {
			if str, ok := s.(string); ok {
				suggested = append(suggested, str)
			}
		}
	} else if v, ok := m["suggested"].([]string); ok {
		suggested = v
	}

	stdCtx := toolCtxAsContext(ctx)
	missionSessionID := ctx.SessionID()
	if missionSessionID == "" {
		return errEnvelope("ask_coordinator", fmt.Errorf("no session id in context"), "internal_error")
	}

	// Sub-agent-only authorization: refuse coord calls. Coord asks
	// the user directly via normal conversation, not via this tool.
	rec, err := t.m.sessionRecord(stdCtx, missionSessionID)
	if err == nil && rec != nil && rec.SessionType == "root" {
		return errEnvelope("ask_coordinator", ErrForbiddenForCoordinator, "forbidden_for_coordinator")
	}

	// Resolve the coord session by walking parent chain to the root.
	coordSessionID := t.m.resolveCoordSession(stdCtx, missionSessionID)
	if coordSessionID == "" || coordSessionID == missionSessionID {
		// Defensive — sub-agents should always have a parent
		// pointing somewhere. Use mission as a last-resort.
		coordSessionID = missionSessionID
	}

	askArgs := map[string]any{"question": question}
	if len(suggested) > 0 {
		askArgs["suggested"] = suggested
	}

	ref, err := t.m.Request(stdCtx, RequestPayload{
		AgentID:          t.m.AgentID(),
		MissionSessionID: missionSessionID,
		CoordSessionID:   coordSessionID,
		ToolName:         "ask_coordinator",
		Args:             askArgs,
		Risk:             RiskMedium,
		Source:           RequestFromAsk,
	})
	if err != nil {
		return errEnvelope("ask_coordinator", err, classifyError(err))
	}

	// Return synthetic waiting-for-answer result so the sub-agent
	// runner ends its turn cleanly. The coord's answer flows back
	// as a NEW tool result on the next dispatch (LLM-driven via
	// constitution; runtime auto-resume is out of scope).
	return map[string]any{
		"ok":          false,
		"status":      "waiting_for_answer",
		"approval_id": ref.ID,
		"hitl_kind":   string(HITLKindAsk),
		"synthetic":   true,
		"message":     fmt.Sprintf("waiting for coordinator answer (id=%s); coord will reply via approval_respond(id, decision=answer, answer=...)", ref.ID),
		"tool":        "ask_coordinator",
		"question":    question,
	}, nil
}

// ─────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────

func stringArg(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// errEnvelope returns the standard error tool result envelope. Match
// pkg/artifacts/tools.go::errEnvelope for consistency across phases.
func errEnvelope(toolName string, err error, code string) (map[string]any, error) {
	return map[string]any{
		"ok":    false,
		"tool":  toolName,
		"error": err.Error(),
		"code":  code,
	}, nil
}

// classifyError maps approvals sentinels to the documented error
// codes in contracts/approval-tools.md.
func classifyError(err error) string {
	switch {
	case errors.Is(err, ErrApprovalNotFound):
		return "approval_not_found"
	case errors.Is(err, ErrAlreadyResolved):
		return "already_resolved"
	case errors.Is(err, ErrApprovalExpired):
		return "expired"
	case errors.Is(err, ErrModifiedArgsMissing):
		return "modified_args_missing"
	case errors.Is(err, ErrAnswerMissing):
		return "answer_missing"
	case errors.Is(err, ErrAnswerOnNonAsk):
		return "answer_on_non_ask"
	case errors.Is(err, ErrInvalidDecision):
		return "invalid_decision"
	case errors.Is(err, ErrForbiddenForSubAgent):
		return "forbidden_for_subagent"
	case errors.Is(err, ErrForbiddenForCoordinator):
		return "forbidden_for_coordinator"
	default:
		return "internal_error"
	}
}
