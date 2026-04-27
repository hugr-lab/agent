# Design 010 — Parallel Async Dispatch + Restart Semantics

**Design**: `010` | **Created**: 2026-04-27 | **Status**: Draft

Companion to design 009 (agent loop redesign). 009 sketched the
fractal-hierarchy model; Phase 1 of that landed in commit `8f4e97d`
(constitution template + `_meta`/`_root` skills + depth tracking).
This design picks up where 009 stopped: **the runtime mechanics that
let multiple sub-agents run in parallel without blocking the parent,
and survive a process restart cleanly**.

It also records the ADK 1.2 research that informs both Phase 2 and
Phase 5 (HITL escalation chain) so a future implementor doesn't
have to redo the spelunking.

## Problem Statement

Three coupled problems, all surfaced when running design-009 Phase 1
scenarios with a real LLM:

1. **`subagent_dispatch` blocks the coordinator's runner.** The tool's
   `Run()` synchronously iterates `for ev, runErr := range r.Run(...)`
   over the child's ADK runner — every turn, every tool call, every
   model token. The coordinator's HTTP request hangs the entire time;
   the DevUI freezes; A2A clients time out. `IsLongRunning() = true`
   is set but unused — ADK's runner doesn't actually defer the call,
   it just records a `LongRunningToolIDs` marker for downstream
   consumers.

2. **No parallel fan-out is possible.** A worker that needs to spawn
   three independent specialists (summariser + schema_explorer +
   data_analyst) emits three `subagent_dispatch` tool_calls in one
   turn. Today they execute serially in ADK's processing loop —
   first finishes, then second, then third. With ~30s per child this
   is 90s wall-clock for work that could be 30s if parallelised.
   Mission_plan with a non-trivial DAG works around this, but only
   for pre-declared graphs; ad-hoc fan-out from worker LLM
   reasoning has no path.

3. **Process restart drops everything in flight.** Goroutines, in-
   memory inboxes, `pendingCalls` maps, dispatcher timeouts — all
   gone on `SIGTERM`. The hub.db has the events but the agent
   process can't tell which sessions need re-spawning, which had
   pending function_responses that never reached the parent, which
   are stuck mid-LLM-call. Today's `RestoreOpen` only rebuilds
   stub sessions; nothing wakes the work back up.

These compound: parallel fan-out is impossible because dispatch is
sync, and restart drops state because the in-memory model is the
authoritative source of truth, not the DB.

## Research

### Existing Solutions / Prior Art

#### ADK 1.2 — `parallelagent` and `sequentialagent` (workflow agents)

Located at `agent/workflowagents/{parallel,sequential}agent`. These are
NOT dynamic dispatch — they're fixed pre-declared workflow patterns
where the agent's `SubAgents()` list is set at build time and the
workflow agent iterates them on `Run()`.

**SequentialAgent** is trivial: `for _, sa := range subAgents { for ev := range sa.Run(ctx) { yield(ev) } }`. No goroutines, no channels.

**ParallelAgent** is more interesting:

```go
errGroup, errGroupCtx := errgroup.WithContext(ctx)
resultsChan := make(chan result)
doneChan := make(chan bool)

for _, sa := range subAgents {
    branch := fmt.Sprintf("%s.%s", curAgent.Name(), sa.Name())
    errGroup.Go(func() error {
        return runSubAgent(subCtx, sa, resultsChan, doneChan)
    })
}

// runSubAgent body:
for ev, err := range agent.Run(ctx) {
    ackChan := make(chan struct{})
    select {
    case results <- result{event: ev, ackChan: ackChan}:
        <-ackChan   // wait until parent's session.AppendEvent done
    case <-done:
        return nil
    }
}
```

Key takeaways for our design:

- **`errgroup.Go` per child.** Concurrency primitive; first error
  cancels the group via shared context.
- **Ack-per-event back-pressure.** The child blocks on `<-ackChan`
  after sending each event; parent closes `ackChan` after persisting
  the event. This prevents reordering when the parent is slow.
- **Branch tagging `<parent>.<child>`.** Naming convention for
  hierarchy IDs; ADK uses it to distinguish writes to the shared
  session.
- **Same `ctx.Session()` across all sub-agents.** ADK's parallel
  pattern writes all child events to the *same* session under
  branch tags. We DO NOT want this — our isolation invariant says
  each sub-agent owns its own session. Reuse the channel/ack
  pattern, not the session-sharing.

#### ADK 1.2 — `tool.Context.FunctionCallID()`

New in 1.2 (was missing in 1.1):

```go
type Context interface {
    agent.CallbackContext
    FunctionCallID() string                       // NEW
    Actions() *session.EventActions
    SearchMemory(context.Context, string) (*memory.SearchResponse, error)
    ToolConfirmation() *toolconfirmation.ToolConfirmation
    RequestConfirmation(hint string, payload any) error
}
```

This is the missing piece for async ticket pattern: the tool's `Run`
can capture the `call_id`, hand it to a goroutine, and the goroutine
can later append a `function_response` event with the matching id.
ADK's runner pairs them automatically on the next `runner.Run` over
the parent session.

#### ADK 1.2 — `Event.LongRunningToolIDs`

```go
// internal/llminternal/base_flow.go
ev.LongRunningToolIDs = findLongRunningFunctionCallIDs(resp.Content, tools)
```

The flag `IsLongRunning() bool { return true }` causes ADK to populate
the slice on the assistant event. A2A's `inputRequiredProcessor`
reads it to convert events into `TaskStateInputRequired` messages
(pause-and-wait). For our internal flow this is the marker that says
"runner: don't auto-respond to this call yet, an external actor will
AppendEvent the response". We rely on this so the parent's runner
loop ends without a response, leaving the function_call hanging.

#### Spec 007 — `mission_executor` + `RunOnce`

Already implements DAG-level async resume:

```go
// pkg/missions/executor/executor.go
RunOnce func(ctx context.Context, coordSessionID string,
              payload graph.CompletionPayload) error
```

When the entire DAG terminates, executor fires `RunOnce` exactly once
on the coordinator session, posting a synthetic `<system: missions
complete>` user message containing `outcomes[]`. The coord's runner
wakes up, sees the marker, summarises.

For Phase 2 we **collapse this from per-DAG to per-call**: one
`RunOnce`-style trigger per child mission completion, posting a
real `function_response` (not synthetic user_message) with the
captured `call_id`.

#### Hugr-backed read tools (cross-cutting)

Sub-agents already have read access to ancestor context via existing
hugr-GraphQL queries:

- `session_search` (skill `_search`): scopes `turn` / `mission` /
  `session` / `user` over `session_events` and the
  `session_notes_chain` view, ranked by relevance × recency.
- `memory_search` (skill `_memory`): retrieves promoted long-term
  memory facts; visibility scopes `self` / `parent` / `ancestors` /
  `agent`.
- `mission_sub_runs` (today via `_root.mission_tools`, Phase 4
  rewires to `child_sessions` references_query): peeks at a child
  session's recent events without polluting parent context.

These tools are **single-source-of-truth queries against hub.db**
(remote: hub Postgres) — they read the same `session_events` and
`memory_notes` tables that the runtime writes to. The actor model
preserves this contract: sub-agents query parent transcript and
shared memory through the same hugr GraphQL surface; the in-memory
inbox does NOT replace these queries, it complements them by
delivering control-plane messages (Completed / FollowUp / Cancel /
HITL) that the read-only tools cannot.

Implications for design:

- A child's `session_search(scope: parent)` reads events directly
  from hub.db. Even if the parent's actor goroutine is busy
  processing other children's Completed messages, the read still
  works — the DB doesn't lock on actor state.
- Visibility scopes (`memory_search(scope: ancestors)`) traverse
  `parent_session_id` chains in SQL. The depth tracking from
  Phase 1 makes this efficient (LIMIT by depth, terminate early).
- Tools like `parent_context(query)` and `parent_recent(n)` (if
  added in Phase 5+) follow the same pattern — pure SQL queries,
  no actor coupling. The runtime ensures the events tables are
  consistent (single-writer per session goroutine, append-only),
  so reads are race-free.

This separation matters for restart semantics: a child resuming
after restart issues a `session_search` against the rehydrated
events, gets the same results the dying process would have, and
proceeds. No need to "replay" any actor-coupled state for read
operations.

### Technical Exploration

#### Current synchronous flow (the thing we're replacing)

```
LLM emits tool_call subagent_dispatch(call_id=X, role=summariser, …)
   ↓
ADK runner invokes subagentDispatchTool.Run(ctx, args)
   ↓
Run() calls dispatcher.RunInternal(parentSessID, …)
   ↓
RunInternal:
   1. m.sessions.Create(child)
   2. childSess.LoadSkill(parentSkill)
   3. build llmagent + adk runner.Runner
   4. for ev, err := range r.Run(runCtx, …, childSessionID, userMsg, …) {
         /* drives every child turn */
      }
   5. d.markChild(childID, "completed")
   6. return DispatchResult{Summary, ChildSessionID, …}
   ↓
Back in tool.Run, return to ADK runner
   ↓
ADK emits function_response{id: X, response: {summary, …}} on parent session
   ↓
Parent LLM continues turn with the response.
```

Wall-clock duration of `Run()` = full child mission duration. Parent
is blocked the whole time.

#### Proposed asynchronous flow

```
LLM emits tool_call subagent_dispatch(call_id=X, role=summariser, …)
   ↓
ADK runner invokes subagentDispatchTool.Run(ctx, args)
   ↓
Run() reads ctx.FunctionCallID() = X
   ↓
Run() calls dispatcher.SpawnAsync(parentSessID, callID=X, role, task)
   ↓
SpawnAsync:
   1. childID := newDispatchSessionID()
   2. m.sessions.Create(child)  // blocks briefly (DB write)
   3. parent.RegisterPendingCall(callID=X, childID=childID)
   4. go childActorLoop(parent, child, callID, …)   // detach
   5. return childID                                  // immediate
   ↓
Run() returns {child_session: childID, status: "running", call_id: X}
   ↓
ADK runner emits function_response{id: X, response: ticket} on parent
ADK runner ALSO emits LongRunningToolIDs=[X] marker on event
   ↓
Parent's runner.Run iterator yields the assistant event and ENDS
(no further LLM turn — ADK sees pending long-running call as
unresolved).

Meanwhile:
   childActorLoop runs r.Run(child) to completion
   On terminal: appendFunctionResponseToParent(parent, callID=X, payload)
                  → parent.session_events row: tool_result with
                    metadata.call_id=X, payload=DispatchResult
                  parent.MarkPendingResolved(X)
                  if parent.PendingEmpty():
                    parent.Resume()   // fires runner.Run on parent
   ↓
Parent's runner.Run sees the new function_response, pairs it with
the long-running call_id=X, continues the model turn.
```

Multiple parallel calls in one parent turn: `LongRunningToolIDs`
contains {X, Y, Z}; each spawns its own child goroutine; each child
appends function_response on completion. Parent stays paused until
all three pending calls resolve, then resumes once.

#### Per-call vs gather-all resume

| Strategy | When parent runs again |
|---|---|
| Per-call resume | After every `function_response` is appended. |
| Gather-all (chosen) | Only when `pendingCalls` set drops to empty. |

Per-call is wrong for ADK semantics: the runner expects every
function_call in the latest assistant event to have a matching
function_response before the next model turn. Resume-on-each
either re-runs the model with partial responses (model sees
"3 calls, 1 has response, 2 pending" — confusing) or requires
ADK to be patched to allow partial. Gather-all matches ADK's
sync-tool semantics exactly, just stretched in time.

#### Pending calls — DB-derived, not in-memory state

The crucial design rule: **`pendingCalls` is a SQL view, not stored
state**. Computing it from `session_events`:

```sql
SELECT call_id FROM events
WHERE session_id = $sid
  AND event_type = 'tool_call'
  AND (metadata->>'long_running')::boolean = true
  AND NOT EXISTS (
    SELECT 1 FROM events r
    WHERE r.session_id = $sid
      AND r.event_type = 'tool_result'
      AND r.metadata->>'call_id' = events.metadata->>'call_id'
  )
```

The actor loop holds a 1s-TTL cache of this query for hot path; on
every `AppendEvent` of a tool_call/tool_result the cache invalidates.
This makes restart automatic: the new process boots, re-runs the
query, gets the same `pendingCalls` set the dying process had, and
the actor loop has correct state with zero special-case code.

### Constraints

1. **ADK-Native principle** (Constitution §I): we cannot fork ADK or
   patch its runner internals. Every async behaviour must be
   expressible as: ADK runner sees events that are valid pairs. We
   add infrastructure *around* ADK, not inside.

2. **Single-writer DuckDB** (local mode): hub.db is a single-process
   DuckDB file. Multiple goroutines writing through `*sessstore.Client`
   must serialise — already handled (the writeMu pattern), but new
   actor goroutines can't bypass it.

3. **Single-instance agent process** (today): we don't run a clustered
   agent fleet. Process supervision (systemd / k8s) ensures one
   instance. We add an advisory lock on startup (`UPDATE sessions SET
   metadata.locked_by=$pid WHERE locked_by IS NULL OR locked_at < now()-5m`) to
   refuse double-start, but don't design for distributed restart.

4. **No new top-level dependencies**: actor loops use stdlib `chan`,
   `sync.WaitGroup`, `context.WithCancel`. Already-in-use
   `golang.org/x/sync/errgroup` is acceptable (ADK uses it).

5. **Events are append-only**: never mutate or delete `session_events`
   rows. Resume after restart can replay; `pendingCalls` derivation
   relies on row presence, not flags.

6. **Idempotency is tool-level**: the runtime guarantees re-firing a
   tool on restart is *possible* (rebuilds the call from events), but
   tools that mutate external state must be idempotent themselves
   (memory_note already uses content hash; artifact_create already
   uses content-derived id; new tools must follow). Out of scope: a
   global idempotency cache.

7. **Read tools stay hugr-GraphQL** (Constitution §I + §V): every
   parent-context / cross-session / memory-search tool is a
   GraphQL query against hub.db. The actor model never replaces a
   read with an in-memory shortcut. This keeps the surface
   uniform between local DuckDB and remote Postgres deployments
   and makes restart/replay correctness automatic.

## Proposed Design

### Architecture

Each session becomes an **actor** with an inbox channel and an owner
goroutine. Sub-agent dispatch becomes message-passing between actors.
ADK runner is invoked from within actor goroutines — runner stays
the LLM driver, the actor coordinates I/O around it.

```
┌─────────────────────────────────────────────────────────┐
│              Process Boundary (one agent)               │
│                                                         │
│  ┌───────────┐                                          │
│  │ root      │  user_message via HTTP/A2A               │
│  │ session   │←──────────────────────────────           │
│  │ actor     │                                          │
│  │           │  inbox: ChildCompleted, FollowUpRouted,  │
│  │           │         CancelChild, HITLAnswer, Resume  │
│  │  runner   │                                          │
│  │  goroutine│                                          │
│  └─────┬─────┘                                          │
│        │ subagent_dispatch tool_call                    │
│        │ → SpawnAsync(child)                            │
│        ▼                                                │
│  ┌───────────┐    ┌───────────┐    ┌───────────┐        │
│  │ child A   │    │ child B   │    │ child C   │        │
│  │ (depth=1) │    │ (depth=1) │    │ (depth=1) │        │
│  │           │    │           │    │           │        │
│  │ own actor │    │ own actor │    │ own actor │        │
│  │ own inbox │    │ own inbox │    │ own inbox │        │
│  │ own runner│    │ own runner│    │ own runner│        │
│  └─────┬─────┘    └─────┬─────┘    └─────┬─────┘        │
│        │                │                │              │
│        ▼                ▼                ▼              │
│   on completion: AppendEvent(parent.events,             │
│                    function_response{id=callID})        │
│                  → parent.inbox <- ChildCompleted{X}    │
│                                                         │
│  ┌─────────────────────────────────────────────────┐    │
│  │  hub.db: sessions, session_events,              │    │
│  │  approvals, notes, memory — single source of    │    │
│  │  truth. Read by hugr GraphQL tools              │    │
│  │  (session_search, memory_search, etc.).         │    │
│  └─────────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────┘
```

Key invariants:

- **One goroutine per active session.** Owns runner.Run loop and
  inbox processing. Drained on shutdown via `wg.Wait(budget)`.
- **Inboxes route control-plane messages, not events.** Events go
  to session's own `session_events` table. Inboxes carry control:
  child completions, follow-up routing decisions, cancel signals,
  HITL answers, resume sentinels.
- **Read tools keep going through hugr GraphQL.** Cross-session
  reads, memory search, sibling lookups — all SQL-backed queries
  against hub.db, unchanged by the actor model. Visibility scopes
  walk `parent_session_id` chains.
- **pendingCalls set is DB-derived.** No authoritative in-memory
  copy. 1s TTL cache for hot path; invalidate on every relevant
  AppendEvent.
- **Resume is `runner.Run` re-invocation.** When pendingCalls drops
  empty, fire `runner.Run` on the parent — ADK reads existing
  events (now containing the function_responses), continues model
  turn.

### Key Interfaces

See `interfaces.go` (sibling file) for compilable Go sketches.

In summary:

- `Session.Inbox` — `chan SessionMessage`, plus close signal.
- `SessionMessage` — sealed interface; variants:
  - `ChildCompleted{CallID, Result, ChildSessionID}` (Phase 2)
  - `FollowUpRouted{Text, OriginEventID}` (preserves spec-007)
  - `CancelChild{CallID, Reason}` (preserves spec-007)
  - `HITLQuestion{ChainID, CallID, Question}` (Phase 5)
  - `HITLAnswer{ChainID, Answer}` (Phase 5)
  - `Resume{}` (sentinel — re-run runner.Run without new input)
- `Dispatcher.SpawnAsync(parentSessionID, callID, ...) (childID string, err error)`
  — non-blocking; returns ticket immediately.
- `Manager.AppendFunctionResponse(sessionID, callID, payload)` — writes
  to session_events with proper metadata + invalidates cache.
- `Manager.Resume(sessionID)` — fires runner.Run if pendingCalls
  empty; no-op otherwise. Idempotent.

### Data Model

#### Additive schema changes

Migration `0.0.6` (additive only — no dropped columns / tables):

```graphql
# session_events.metadata is already a free JSON map; we standardise
# two keys for long-running flow:
#
# tool_call event:
#   metadata.call_id      string  — ADK function_call ID
#   metadata.long_running bool    — true when tool.IsLongRunning()
#
# tool_result event:
#   metadata.call_id      string  — matches the tool_call's call_id
#
# sessions:
#   metadata.last_runner_at  RFC3339 string  — for stale detection
#   metadata.locked_by       int  (pid)      — process advisory lock
#   metadata.locked_at       RFC3339 string  — lock acquired ts
```

PostgreSQL-only indexes (gated by `{{ if isPostgres }}` per project
rule — DuckDB stays index-free):

```sql
CREATE INDEX idx_events_call_id ON session_events
  USING btree ((metadata->>'call_id'))
  WHERE event_type IN ('tool_call', 'tool_result');

CREATE INDEX idx_sessions_locked_by ON sessions
  USING btree ((metadata->>'locked_by'))
  WHERE status = 'active';
```

#### No new tables

The actor model adds no persistent tables. `pendingCalls` is derived
by the SQL view above. Inboxes are in-memory only — restart drops
in-flight messages, but the underlying state (pendingCalls,
session_events) is recomputable.

### Restart Semantics

Booted process must restore active sessions to a working state. The
flow is bottom-up over the depth tree, and the actor inbox cascade
naturally rewires parent-child relationships as goroutines come up.

#### Boot sequence

```
1. Acquire process lock:
   UPDATE sessions
     SET metadata.locked_by=$pid, metadata.locked_at=now()
   WHERE locked_by IS NULL
      OR locked_at < now() - INTERVAL '5 minutes';
   If 0 rows updated AND there exist locked rows by another live
   pid → abort startup with "another agent instance running".

2. Restore session stubs (existing Manager.RestoreOpen):
   For every row in sessions WHERE status='active':
     - Build Session struct (depth, parent, mission, metadata).
     - Create inbox.
     - Register in m.sessions map.
   For every parent-child relation (parent_session_id != ''):
     - parent.children[child.callID] = child stub
     (callID derived from the parent's tool_call event; see step 4)

3. Classify each session by last meaningful event:
   Read last 10 events of each restored session. Apply Case A-F
   below. Decision per session is one of:
     - {markCompleted, emitChildCompletedToParent}  (Case A)
     - {ignore — already resolved}                   (Case B)
     - {syntheticToolError, runnerResume}            (Case C)
     - {addToRespawnList}                            (Case D)
     - {syntheticChildFailed}                        (Case E)
     - {markAbandoned, emitChildFailedToParent}      (Case F — timeout)

4. Bottom-up spawn goroutines:
   Sort respawnList descending by depth. For each session:
     - Spawn actor goroutine (it will run runner.Run loop).
     - Actor reads its inbox, processes any inherited messages
       (in particular ChildCompleted from step 3's emissions).

5. After all goroutines started, the inbox cascade settles:
   - Leaves complete (or are already terminal) → emit Completed
     up to their parents.
   - Parents accumulate Completed messages, AppendFunctionResponse
     to their events, decrement their derived pendingCalls.
   - When parent's pendingCalls drains, parent's actor calls
     runner.Run again, continues mid-turn.
   - Recurses up to root.
```

#### Case classification

Read the last events of each active session. Pick the first matching
case top-down:

| Case | Last event pattern | Action |
|---|---|---|
| A | Final `llm_response` (no pending tool_calls in same content) | Session is logically complete. Mark `status=completed`. If parent exists, emit `ChildCompleted` to parent's inbox. |
| B | `tool_result` matching every prior `tool_call` (no pending calls in last assistant turn) | Session is paused on next user message. Idle. No spawn — will run on next user_message append. |
| C | `tool_call` (sync, non-long-running) without a `tool_result` | In-flight tool died mid-execution. Append a synthetic `tool_result` with `metadata.error="interrupted by restart"`. Spawn runner; LLM decides retry/give-up per its skill instructions. |
| D | `tool_call` long_running (subagent_dispatch) without `tool_result`, AND child session exists | Child must continue. Add child to respawnList. Parent's pendingCalls remains; parent's actor will eventually consume the ChildCompleted from the resumed child. |
| E | `tool_call` long_running without `tool_result` AND child session missing/never created | Child failed before start. Append synthetic `tool_result` with `metadata.error="child failed before start"` and `metadata.call_id=X`. |
| F | Any case where `last_event_at < now() - 24h` | Stale. Mark `status=abandoned` with reason `restart-stale`. Cascade to children. |

#### Edge cases

**Mid-LLM-call interrupted (the worst case).** HTTP request to the
LLM provider was cancelled mid-stream; no `llm_response` event
exists. Last event is `user_message` (Case E). Restart spawns
runner; runner re-invokes the LLM with the same input. **Cost**:
re-pay the tokens. Mitigation: graceful shutdown (SIGTERM with
timeout) drains via `wg.Wait(budget)` before forcing cancel —
matches existing executor pattern.

**Child completed, function_response appended to parent, parent
crashed before reading inbox.** Parent restarts: events table has
the function_response (it's persisted). Case B applies (no pending
calls — they all resolved). Parent's runner runs and continues.
Self-healing.

**Two concurrent agent instances.** Step 1's advisory lock prevents
this. The lock is timestamp-based (5min stale window) so a hard-
crashed instance's lock is taken over by the next start.

**Orphan children (parent abandoned).** Periodic sweep:

```sql
UPDATE sessions SET status='abandoned',
  metadata.abandon_reason='orphan'
WHERE status='active'
  AND parent_session_id IN (
    SELECT id FROM sessions
    WHERE status IN ('abandoned', 'failed')
  );
```

Run on a 5min ticker. Cascade abandons happen via the actor model
in normal operation; this is the cleanup for cases the actor flow
missed (e.g. parent's actor crashed before emitting cascade).

**Worker pool exhaustion.** If 1000 sessions need respawning at
boot, don't launch 1000 goroutines all racing for LLM clients. Use
a worker pool with semaphore (size = N from config, default 16).
Sessions wait in the spawn queue; when a goroutine slot frees, the
next session's runner starts.

**Pending HITL chain on restart (Phase 5).** A `hitl_question` event
is on the parent's events; the pending answer is in metadata. After
restart the parent's actor reads its events (Case B if no other
pending tool_call), sees the unresolved question, and waits on its
inbox for an HITLAnswer. The answer eventually arrives via the same
chain mechanism (user → root → ... → parent).

**Pending approval on restart.** Already handled by `approvals`
table (spec 009) — RestoreOpen scans pending rows and the gate
callback consults them. The new piece: when an approval flips, the
gate's "release" path emits a tool_result event AND a
`Resume{}` message to the actor. Actor sees Resume, runs runner.

**Cross-session reads during restart.** If a child issues
`session_search(scope: parent)` while the parent is mid-restart,
the query reads from hub.db directly — gets whatever events were
persisted by the dying process. If the parent appended a partial
event before crash, the child sees it. If not, it doesn't.
Either way the read is consistent. Actor goroutines never gate
DB reads.

### Dependencies

Stdlib:
- `context.Context` (already used)
- `sync.WaitGroup`, `sync.Mutex` (already used)
- `sync/atomic` for the cache version counter

Already-vendored:
- `golang.org/x/sync/errgroup` (used by ADK; we use it for the
  spawn pool)

ADK 1.2:
- `tool.Context.FunctionCallID()` — required, exists in 1.2
- `Event.LongRunningToolIDs` — required, exists in 1.2
- `session.Service.AppendEvent` — required, our `Manager`
  already implements this

Hugr GraphQL (existing):
- `session_events`, `sessions`, `session_notes_chain` — read by
  `_search` skill tools, unchanged.
- `memory_notes` with scope filter — read by `_memory` skill,
  unchanged.

No new top-level dependencies. Conforms to Constitution §V.

## Trade-offs & Alternatives

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **Actor mailbox per session (chosen)** | Symmetric for parent-child + future HITL chain. DB-derived state ⇒ free restart. Reuses ADK runner per session. | New abstraction layer. ~600-800 LOC new code. | CHOSEN |
| Sync dispatch with goroutine + WaitGroup (no inboxes) | Smaller diff; just spawn goroutine, sync.WaitGroup at parent. | Doesn't generalise to follow-up routing or HITL chain. Restart still drops state because no DB-derivation. Only fixes UI freeze. | REJECTED — solves only one of three problems. |
| Per-call resume (each ChildCompleted fires runner immediately) | Simpler: no "wait for pending empty" logic. | Breaks ADK pair-matching when N parallel calls outstanding. Model sees partial results, may issue more tool_calls based on partial state, hard to reason about. | REJECTED — wrong for ADK semantics. |
| External job queue (e.g. dedicated worker process per child) | Decouples agent from worker lifecycle; restart of agent doesn't kill workers. | Adds inter-process IPC, queue infra, deployment complexity. We're a single-binary today; not worth it. | REJECTED — out of constitution scope (V Minimal Dependencies). |
| ADK's parallelagent + shared session | Reuses ADK 1.2 directly, ack-per-event proven. | Loses transcript isolation (all children write to same session under branch tags). Breaks our `parent_session_id` linkage and `child_sessions` references_query. Doesn't help: workflow agents are pre-declared, not LLM-driven dynamic dispatch. | REJECTED — wrong shape for tool-driven dispatch. |
| Sync dispatch + UI streaming (show "running" status without blocking) | No runtime change, just plumbing. | Doesn't enable parallel fan-out. Still blocks the model loop. Doesn't help restart. | REJECTED — cosmetic fix only. |
| In-memory IPC for parent-context reads (replace hugr query with actor lookup) | Faster reads in same-process case. | Breaks restart (in-memory state lost), breaks remote-mode (no hugr in path), breaks visibility-scope semantics tied to SQL. | REJECTED — violates §I/§V/§7. |

## Open Questions

1. **Worker pool size default.** Likely `runtime.NumCPU() * 2`
   for I/O-bound LLM calls, capped at 32. Make it configurable
   via `cfg.Sessions.WorkerPoolSize`. Phase 2 implementation
   should benchmark before fixing the default.

2. **Cache invalidation strategy for pendingCalls.** Two options:
   (a) version counter incremented on every relevant AppendEvent;
   actor compares snapshot version; (b) flat 1s TTL that
   over-fetches under load. (a) is correct, (b) is simpler. Pick
   during implementation.

3. **HITL chain `chain_id` allocation.** Each `ask_user` call
   creates a fresh chain_id. Escalation re-uses the parent's
   chain_id but adds an `escalated_from` link. Need to clarify in
   Phase 5: can a single user-question resolve multiple chains
   (e.g. fan-out re-asks the same thing)? Conservative answer:
   no, one question = one chain.

4. **Function_response payload size cap.** A child's summary can be
   1500+ tokens (`summary_max_tokens`). Today we put it inline in
   the function_response. With multiple parallel children, that
   inflates the parent's prompt. Mitigation: link to a child's
   `agent_result` event by id and let parent fetch on demand via
   `session_search`. Out of Phase 2 scope; revisit when context-
   budget pressure surfaces.

5. **A2A streaming during async dispatch.** Current A2A response
   waits for the full coord turn. With async dispatch, the
   "run coord, dispatch children, wait, resume coord" cycle could
   take minutes. Either A2A holds the connection for the whole
   duration, or we surface progress via TaskStateWorking
   intermediate events. Out of Phase 2 scope; documented for
   Phase 3 / future UI work.

6. **Restart-time skill-state replay.** Sessions have skills loaded
   into memory (active providers, references). The current
   `ensureMaterialized` rebuilds this on first Get from the events
   stream. With actor goroutines spawning at boot, materialization
   happens before the first tool call. Need to verify
   materialization is goroutine-safe (it uses `sync.Once` per
   session — already safe).

7. **`session_search(scope: parent)` consistency under parent
   restart.** Child queries while parent's actor is mid-restart.
   The query reads `session_events` directly so it's race-free at
   the SQL level, but the result might differ from a query 1s
   later (parent may have appended events in between). This is
   normal eventually-consistent read behaviour and matches
   today's semantics; document it explicitly so callers don't
   assume snapshot isolation.

## Spec Input

> Copy this block as input to `/speckit.specify`.

```
Phase 2 of design 010: parallel async dispatch + restart semantics.

Behaviour the user sees:
- subagent_dispatch (and prebound subagent_<skill>_<role>) returns a
  ticket within ~50ms instead of blocking for the child's full
  duration.
- The coordinator can issue multiple subagent_dispatch tool_calls in
  one turn; they run in parallel; the model gets all responses
  together when the slowest finishes.
- Existing read tools (session_search, memory_search,
  mission_sub_runs / child_sessions) continue to work unchanged —
  same hugr-GraphQL surface, same visibility scopes.
- The DevUI / A2A client receives progress events ("dispatched
  child_xxx") and a final consolidated response.
- On agent process restart (SIGTERM/crash), active sessions resume
  where they left off: in-flight children continue, pending parents
  receive their function_responses, the runner picks up the model
  turn at the right place. No conversation is lost.
- A stale-session sweep (>24h idle) marks abandoned with reason on
  restart; cascade to dependent children.

Behaviour the operator sees:
- A new advisory lock on `sessions.metadata.locked_by` prevents
  double-instance startup.
- Configurable worker pool (`sessions.worker_pool_size`, default
  `NumCPU*2` capped 32) bounds concurrent runner goroutines.
- New schema-additive metadata keys: tool_call.metadata.call_id,
  tool_call.metadata.long_running, tool_result.metadata.call_id,
  sessions.metadata.{last_runner_at, locked_by, locked_at}.
- One Postgres-only index per spec on session_events.metadata.call_id.

Out of scope (later phases):
- HITL escalation chain (Phase 5 — uses same primitive).
- Removal of pkg/missions/graph (Phase 4).
- RequireConfirmation tool gates (Phase 3).
- Cross-process / clustered restart (not planned).
- skill_builder + user-skill source (Phase 6, design 011).
```
