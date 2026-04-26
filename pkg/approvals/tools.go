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
