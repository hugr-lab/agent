# Design 010 — Parallel Async Dispatch + Restart

Companion design to 009 (agent loop redesign). Picks up where 009's
Phase 1 stopped (constitution template + `_meta`/`_root` skills +
depth tracking, shipped in `8f4e97d`) and lays out the runtime
mechanics for Phase 2 onwards.

## Contents

- [`design.md`](design.md) — the full design document. Problem
  statement, ADK 1.2 research, proposed actor-mailbox architecture,
  restart semantics, trade-offs, open questions.
- [`interfaces.go`](interfaces.go) — Go interface sketches for
  `Inbox` / `SessionMessage` / `AsyncDispatcher` / `AsyncManager` /
  restart classification. Not imported by production code; anchors
  the design.

## Scope

| In | Out |
|---|---|
| Phase 2: parallel `subagent_dispatch` via actor inboxes | Phase 3: RequireConfirmation tool gates (separate design) |
| Process restart semantics (DB-derived state) | Phase 4: removal of `pkg/missions/graph` (deletion cleanup) |
| Schema-additive metadata for `call_id` / `long_running` / `last_runner_at` / `locked_by` | Phase 5: HITL escalation chain (uses Phase 2 primitives, separate design) |
| ADK 1.2 research (`FunctionCallID`, `LongRunningToolIDs`) | Phase 6: `skill_builder` + user-skill source |

## Status

Draft. Awaiting `/speckit.specify` to convert the Spec Input block
into a feature spec.

## Why this exists

Three problems coupled into one design because solving any in
isolation produces churn:

1. Sub-agent dispatch blocks the coordinator's runner → UI freeze.
2. No way to fan out N parallel children from a single tool turn.
3. Process restart drops in-flight goroutines and inboxes.

The actor-mailbox model with DB-derived `pendingCalls` solves all
three at once: each session is a goroutine with an inbox, parallel
spawn is N inboxes / N goroutines, and restart is automatic
because everything that matters lives in `session_events`.
