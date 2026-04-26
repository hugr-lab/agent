package approvals

import (
	"context"
	"fmt"
)

// Gate is the consumer-facing decision-point inserted into the
// dispatcher's tool-call path. Methods are receiver functions on
// *Manager via this thin wrapper — no allocation per call beyond
// what Manager.Request already does.
type Gate struct {
	m *Manager
}

// GateDecision is the Gate's verdict on a single tool call. (The
// user-supplied decision enum is `Decision` in refs.go; the two are
// distinct.)
type GateDecision struct {
	Action     DecisionAction
	Reason     string // human-readable; appears in logs and audit blob
	ApprovalID string // populated when Action == DecisionManual
	Risk       Risk   // populated for manual/denied; default Medium
}

// DecisionAction is the verdict enum.
type DecisionAction int

const (
	// DecisionAllow — proceed to the actual tool invocation.
	DecisionAllow DecisionAction = iota
	// DecisionManual — pause the mission; an approvals row at
	// .ApprovalID has already been inserted by the Gate via
	// Manager.Request.
	DecisionManual
	// DecisionDeny — refuse the call. The dispatcher should return an
	// error tool result to the sub-agent without inserting an
	// approvals row.
	DecisionDeny
)

// selfAuthenticatingTools is the hardcoded recursion-guard set. The
// four tools listed here are ALWAYS allowed by the Gate, regardless
// of policy / approval_rules state. Without this guard, the user
// would have no way to respond to or modify their own pending
// approvals.
//
// SECURITY NOTE: this set MUST stay small. No configuration path
// should be able to extend or shrink it.
var selfAuthenticatingTools = map[string]struct{}{
	"approval_respond": {},
	"policy_list":      {},
	"policy_set":       {},
	"policy_remove":    {},
}

// Check inspects a tool call and returns the Gate's decision. Side
// effects ONLY occur on DecisionManual: an approvals row is inserted
// and the mission is transitioned to `waiting`.
//
// Decision flow:
//
//  1. Recursion guard — self-authenticating tools always Allow.
//  2. safe_policy_change widening detector — when the tool is
//     `policy_set` and the requested change widens permissions on a
//     currently-`manual_required` tool, route the policy_set itself
//     through require_user (meta-approval). Skipped on resume from a
//     meta-approval (InternalBypass.SafePolicyChange = true).
//  3. PolicyStore resolution chain — first match wins.
//  4. (unreachable) — Resolve always returns one of the three
//     enum values.
func (g *Gate) Check(ctx context.Context, call ToolCall) GateDecision {
	if g == nil || g.m == nil {
		return GateDecision{Action: DecisionAllow, Reason: "gate not configured"}
	}

	// Step 1 — recursion guard.
	if _, guarded := selfAuthenticatingTools[call.ToolName]; guarded {
		return GateDecision{
			Action: DecisionAllow,
			Reason: "self-authenticating tool",
		}
	}

	// Step 2 — safe_policy_change widening detector. US2 implements
	// the full body; for US1 the manager's PolicyStore stub never
	// reports manual_required, so this branch is a no-op except for
	// the structural check.
	if call.ToolName == "policy_set" && !call.InternalBypass.SafePolicyChange {
		safe := true
		if g.m.cfg.SafePolicyChange != nil {
			safe = *g.m.cfg.SafePolicyChange
		}
		if safe {
			if dec := g.checkSafePolicyChange(ctx, call); dec.Action != DecisionAllow {
				return dec
			}
		}
	}

	// Step 3 — policy resolution chain.
	resolved := g.m.policy.Resolve(ctx, call)
	switch resolved.Policy {
	case PolicyAlwaysAllowed:
		return GateDecision{Action: DecisionAllow, Reason: resolved.Origin}
	case PolicyDenied:
		return GateDecision{
			Action: DecisionDeny,
			Reason: resolved.Origin,
			Risk:   RiskHigh,
		}
	case PolicyManualRequired:
		risk := RiskMedium
		if call.Frontmatter != nil {
			if r, ok := call.Frontmatter.RiskOverrides[call.ToolName]; ok {
				risk = r
			}
		}
		appID, err := g.requestManual(ctx, call, risk, resolved.Origin)
		if err != nil {
			return GateDecision{
				Action: DecisionDeny,
				Reason: fmt.Sprintf("approvals: insert: %v", err),
			}
		}
		return GateDecision{
			Action:     DecisionManual,
			Reason:     resolved.Origin,
			ApprovalID: appID,
			Risk:       risk,
		}
	}
	// Unreachable: Resolve always returns one of the three.
	return GateDecision{Action: DecisionAllow, Reason: "unreachable default"}
}

// checkSafePolicyChange handles Gate Step 2 — widening detection.
// When a policy_set call would widen `manual_required` →
// `always_allowed` on the target tool, we route the policy_set
// itself through require_user.
func (g *Gate) checkSafePolicyChange(ctx context.Context, call ToolCall) GateDecision {
	target, _ := call.Args["tool_name"].(string)
	newPolicy, _ := call.Args["policy"].(string)
	if target == "" || newPolicy != "always_allowed" {
		return GateDecision{Action: DecisionAllow}
	}
	current := g.m.policy.Resolve(ctx, ToolCall{
		ToolName: target,
		Skill:    call.Skill,
		Role:     call.Role,
	})
	if current.Policy != PolicyManualRequired {
		return GateDecision{Action: DecisionAllow}
	}
	// Widening detected — meta-approval required. The widened tool
	// is `policy_set`; the args carry the original policy_set
	// invocation verbatim so the executor can replay them on resume
	// with InternalBypass.SafePolicyChange = true.
	appID, err := g.requestManual(ctx, ToolCall{
		AgentID:        call.AgentID,
		SessionID:      call.SessionID,
		CoordSessionID: call.CoordSessionID,
		ToolName:       "policy_set",
		Args:           call.Args,
	}, RiskHigh, "safe_policy_change widening "+target)
	if err != nil {
		return GateDecision{
			Action: DecisionDeny,
			Reason: fmt.Sprintf("approvals: insert meta: %v", err),
		}
	}
	return GateDecision{
		Action:     DecisionManual,
		Reason:     "safe_policy_change widening manual_required → always_allowed",
		ApprovalID: appID,
		Risk:       RiskHigh,
	}
}

// requestManual is the shared insert path used by Steps 2 + 3 when
// the Gate decides Manual. Returns the new approvals row id.
func (g *Gate) requestManual(ctx context.Context, call ToolCall, risk Risk, _ string) (string, error) {
	ref, err := g.m.Request(ctx, RequestPayload{
		AgentID:          call.AgentID,
		MissionSessionID: call.SessionID,
		CoordSessionID:   call.CoordSessionID,
		ToolName:         call.ToolName,
		Args:             call.Args,
		Risk:             risk,
		Source:           RequestFromGate,
		Skill:            call.Skill,
		Role:             call.Role,
		Frontmatter:      call.Frontmatter,
	})
	if err != nil {
		return "", err
	}
	return ref.ID, nil
}
