---
name: _approvals
version: "0.1.0"
description: >
  HITL approvals + tool policies + sub-agent escalation surface.
  Coordinator-side tool surface for resolving pending approvals
  and managing persistent tool policies; sub-agent-side surface
  for escalating ambiguous decisions via ask_coordinator. Wired
  on every session; the runtime gate intercepts sub-agent tool
  calls before dispatch and routes destructive / role-gated tools
  through this surface.
autoload: true
autoload_for: [root, subagent]
providers:
  - name: _approvals
    provider: _approvals
---

# Approvals

The approvals surface lives at the boundary between the coordinator
(your decisions) and sub-agents (their tool calls). It does three
jobs:

- **Gate destructive tool calls.** When a sub-agent role declares
  `approval_rules.require_user: [<tool>]` (or an operator policy
  resolves to `manual_required`), the runtime pauses the mission
  and surfaces an envelope to the user via your session. You
  relay the user's reply through `approval_respond`.
- **Tune policies.** Persistent overrides for tool-call gates land
  in `tool_policies` via the `policy_*` tools. Widening a
  `manual_required` tool to `always_allowed` requires user
  approval itself (safety net against prompt injection).
- **Carry sub-agent questions.** A sub-agent stuck on ambiguity
  calls `ask_coordinator(question, suggested?)` — its session
  pauses and the question lands on yours as an envelope with
  `hitl_kind=ask`. You either answer from your own knowledge or
  relay to the user; either way `approval_respond(decision=answer,
  answer=...)` resumes the sub-agent with the answer.

## Tools (US1 — phase-4 foundation)

This skill currently exposes:

- **`pending_approvals(limit?)`** — coordinator-only. Returns the
  open approval rows on your session with their canonical `app-`
  ids, tool names, risks, mission ids, args excerpts, ages, and
  the legal reply choices. **Call this whenever the user
  references an approval** ("approve the cleanup", "reject it",
  "approve app-7c") to resolve the canonical id before invoking
  `approval_respond`. Don't try to find the id by scanning event
  history — the runtime does NOT auto-inject pending state into
  your prompt; this tool is the source of truth.
- **`approval_respond`** — coordinator-only. Translates the user's
  free-form reply (`approve <id>`, `reject <id> because <reason>`,
  `modify <id> {<json>}`, `answer <id> <text>`) into a structured
  decision. Returns `{ok, status}`. Errors when the id is missing,
  already resolved, or expired.

Phase-4 follow-ups will add:

- `policy_list / policy_set / policy_remove` (US2)
- `ask_coordinator` (US3)

## Recognising user replies

When you see an `approval_requested` event followed by a user
message, parse the reply:

| User says | Tool call |
|---|---|
| `approve app-7c9d` | `approval_respond(id="app-7c9d", decision="approve")` |
| `approve app-7c9d looks fine` | `approval_respond(id, decision="approve", note="looks fine")` |
| `reject app-7c9d` | `approval_respond(id, decision="reject")` |
| `reject app-7c9d because the date cutoff is wrong` | `approval_respond(id, decision="reject", note="the date cutoff is wrong")` |
| `modify app-7c9d {"statement": "..."}` | `approval_respond(id, decision="modify", modified_args={...})` |
| `answer app-8a0c use tf.incidents` | `approval_respond(id, decision="answer", answer="use tf.incidents")` |

Partial id matching: when the user types `app-7c`, look up the
unique pending approval visible to you and use its canonical id.
If multiple pending rows match, ask the user to disambiguate as a
normal turn — do not call `approval_respond`.

`modify` without new args is invalid. If the user says "modify it"
without supplying or implying new args, ask in the next turn before
calling the tool.

When the user's message does NOT reference an approval id (no
`app-...` prefix, no recognised verb), treat it as a normal
conversation turn. Pending approvals continue ticking toward their
timeout; the user can ignore them or come back to them later.

## Scope

The Gate is enforced for **sub-agent tool calls only**. Coordinator
tools (your own surface) do not pass through the Gate — the
recursion guard hardcodes `approval_respond` and the future
`policy_*` tools as always-allowed so you can never lock yourself
out of resolving a pending approval.
