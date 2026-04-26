// Package approvals owns the HITL approval subsystem (spec 009
// phase 4). It implements the persistent approvals + tool_policies
// state, the Gate consulted by pkg/missions/executor on every
// sub-agent tool call, the coordinator-facing approval/policy tools,
// the sub-agent ask_coordinator escalation tool, and the timeout
// sweeper that runs on pkg/scheduler.Cron.
//
// Invariant cheatsheet:
//
//   - Recursion guard. The four self-authenticating tools
//     (approval_respond, policy_list, policy_set, policy_remove)
//     short-circuit at the top of Gate.Check; they NEVER pass
//     through the policy / approval-rules chain. The set is
//     hardcoded in gate.go and cannot be overridden by config.
//
//   - Atomic policy cache. PolicyStore holds an
//     atomic.Pointer[PolicySnapshot]. Resolve is lock-free; Set /
//     Remove serialize on a writer mutex and atomic-swap the
//     snapshot pointer. Readers never see a partial snapshot.
//
//   - safe_policy_change. A policy_set that would widen
//     manual_required → always_allowed for the currently-resolved
//     policy of the target tool itself runs through require_user
//     before taking effect. Recursion guard exempts policy_set from
//     being itself blocked by manual_required; the widening
//     detector is an orthogonal Step 2 in the gate. Resume after
//     the meta-approval bypasses the detector via
//     ToolCall.InternalBypass.SafePolicyChange = true (executor-set
//     only; never LLM-exposed).
//
//   - Restart safety. Pending approval rows survive process
//     restart. Mission resume happens on the scheduler tick — the
//     tick polls missions WHERE status='waiting' and joins to
//     approvals to find resolutions. No goroutines wait on Go
//     channels for row flips.
//
//   - Synthetic tool result. When the Gate decides Manual, the
//     executor returns a structured ADK tool result to the
//     sub-agent's runner so the model turn ends cleanly. The
//     mission's status flips to `waiting` in the same DB
//     transaction as the approvals insert + lifecycle events.
//
//   - Bytes never duplicated. Tool args live on the approvals row
//     as JSON; the approval_requested event metadata carries an
//     args_digest (preview), not the full args.
//
//   - Indexes are Postgres-only. Migration 0.0.5 ships zero
//     indexes for both new tables on the DuckDB branch; Postgres
//     keeps composite indexes on (agent_id, coord_session_id,
//     status) and (agent_id, scope).
//
// See specs/009-hitl-search-composition/contracts/ for the full
// contract surface.
package approvals
