package approvals

import (
	"context"
	"fmt"

	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/tool"
)

// GateCallbackConfig captures the per-mission context the Gate needs
// for resolution-chain debugging + frontmatter consultation. Built
// once per sub-agent dispatch in pkg/agent.Dispatcher.runInternal
// and threaded into llmagent.New via BeforeToolCallbacks.
type GateCallbackConfig struct {
	// AgentID scopes the gate. Required.
	AgentID string

	// MissionSessionID is the sub-agent's own session id. Used as
	// `mission_session_id` on the approvals row. Required.
	MissionSessionID string

	// CoordSessionID is the surfacing coordinator's session id (the
	// root of the mission graph). Required.
	CoordSessionID string

	// Skill / Role identify the role spec; used for the resolution
	// chain's `role:<skill>:<role>` and `skill:<skill>` scopes.
	Skill string
	Role  string

	// Frontmatter is the role's `approval_rules` declaration parsed
	// from skills/<skill>/SKILL.md frontmatter. Optional — nil ⇒ the
	// resolution chain falls through frontmatter step to hardcoded
	// default.
	Frontmatter *FrontmatterApprovalRules
}

// GateCallback returns a llmagent.BeforeToolCallback that consults
// the Gate before every tool dispatch. ADK semantics:
//
//   - Returning (nil, nil) → ADK runs the tool normally.
//   - Returning a non-nil result map → ADK uses it AS the tool
//     result and skips Run.
//   - Returning a non-nil error → ADK reports tool error.
//
// Decision mapping:
//
//   - DecisionAllow → return (nil, nil)
//   - DecisionManual → return synthetic tool result with the
//     waiting-for-approval message + approval_id. The mission row
//     has already been transitioned to `waiting` by the side effect
//     of Manager.Request inside Gate.Check.
//   - DecisionDeny → return error envelope as the tool result so
//     the LLM sees the refusal cleanly without crashing the runner.
//
// The callback only intercepts SUB-AGENT tool calls. The coordinator's
// llmagent does NOT install this callback (its own approval_respond
// / policy_* tools are self-authenticating per the recursion guard,
// and the design explicitly scopes the gate to sub-agent tool calls).
func GateCallback(m *Manager, cfg GateCallbackConfig) (llmagent.BeforeToolCallback, error) {
	if m == nil {
		return nil, fmt.Errorf("approvals: GateCallback requires non-nil Manager")
	}
	if cfg.MissionSessionID == "" {
		return nil, fmt.Errorf("approvals: GateCallback requires MissionSessionID")
	}
	if cfg.CoordSessionID == "" {
		return nil, fmt.Errorf("approvals: GateCallback requires CoordSessionID")
	}
	if cfg.AgentID == "" {
		cfg.AgentID = m.AgentID()
	}
	gate := m.Gate()

	return func(toolCtx tool.Context, t tool.Tool, args map[string]any) (map[string]any, error) {
		ctx := contextFromToolCtx(toolCtx)
		decision := gate.Check(ctx, ToolCall{
			AgentID:        cfg.AgentID,
			SessionID:      cfg.MissionSessionID,
			CoordSessionID: cfg.CoordSessionID,
			ToolName:       t.Name(),
			Args:           args,
			Skill:          cfg.Skill,
			Role:           cfg.Role,
			Frontmatter:    cfg.Frontmatter,
		})
		switch decision.Action {
		case DecisionAllow:
			return nil, nil
		case DecisionManual:
			return syntheticManualResult(t.Name(), decision.ApprovalID), nil
		case DecisionDeny:
			return errEnvelopeFromGate(t.Name(), decision.Reason), nil
		default:
			// Unreachable; treat as Allow rather than crashing.
			return nil, nil
		}
	}, nil
}

// syntheticManualResult is the structured ADK tool result returned to
// the sub-agent's runner when the Gate decides Manual. The model
// turn ends cleanly; the executor's resume path will re-feed the
// original (or modified) args after the user responds.
//
// The "synthetic" key marks this as a Gate-injected response so debug
// tools and scenario assertions can distinguish it from real tool
// results.
func syntheticManualResult(toolName, approvalID string) map[string]any {
	return map[string]any{
		"ok":          false,
		"status":      "waiting_for_approval",
		"approval_id": approvalID,
		"hitl_kind":   string(HITLKindApproval),
		"synthetic":   true,
		"message": fmt.Sprintf(
			"waiting for approval (id=%s); reply with approval_respond when resolved",
			approvalID,
		),
		"tool": toolName,
	}
}

// errEnvelopeFromGate is the structured ADK tool result returned when
// the Gate decides Deny. The sub-agent's LLM sees a clear refusal
// and can either retry with different args (if the policy is name-
// scoped and a different tool would be allowed) or abandon the path.
func errEnvelopeFromGate(toolName, reason string) map[string]any {
	return map[string]any{
		"ok":    false,
		"tool":  toolName,
		"error": "gate denied: " + reason,
		"code":  "policy_denied",
	}
}

// contextFromToolCtx extracts the standard library context.Context
// out of an ADK tool.Context. ADK's tool.Context carries cancellation
// + deadline state via its own machinery; for our purposes (DB
// lookups + event writes) we just need the underlying Context for
// store calls. ADK's tool.Context implements context.Context so
// using it directly is fine.
func contextFromToolCtx(toolCtx tool.Context) context.Context {
	if c, ok := any(toolCtx).(context.Context); ok {
		return c
	}
	// Fallback — should not happen in practice; return Background()
	// to avoid a nil-context panic in store calls.
	return context.Background()
}
