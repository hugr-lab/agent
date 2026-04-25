# Scenario runner

Data-driven harness for driving the **full** hugr-agent runtime
against a live LLM + embedder. Not a unit test — output is
non-deterministic, so scenarios are observational: they feed a
fixed list of user messages through the runtime, run GraphQL
queries at each step, and leave `hub.db` on disk for manual
inspection.

## Why

Unit tests cover contracts and race conditions. They cannot catch
things like:

- a real LLM refusing to call a tool the prompt is clearly asking
  for,
- a prompt-rendering bug where the data is present but the section
  heading confuses the model,
- a hub.db write that silently swallows rows on real-world input
  (e.g. apostrophes in tool args),
- a compactor that never fires because it miscounts the prompt.

Scenarios exist to poke all that end-to-end against the same stack
production runs: full `pkg/runtime.Build`, real skills, real memory
tables, real embedder. When something looks off, the leftover
`hub.db` gets inspected with DuckDB (`duckdb -readonly …`) and the
transcript is read like a log.

## Layout

```
tests/scenarios/
├── README.md                 ← you are here
├── config.yaml               ← base config (mirrors prod config.yaml shape)
├── .test.env.example         ← copy to .test.env and fill
├── .test.env                 ← gitignored, holds LLM_LOCAL_URL / EMBED_LOCAL_URL
├── runner_test.go            ← TestScenarios — walks *.yaml + plays each
├── harness/
│   ├── agent.go              ← Setup() → *harness.Agent
│   ├── runner.go             ← RunTurn + per-event log
│   ├── inspect.go            ← Inspector — sessions / events / notes + Dump
│   └── skills/domain/        ← fixture SKILL.md with sub_agents.summariser
├── .data/                    ← per-run hub.db artefacts (gitignored)
└── <name>/
    ├── scenario.yaml         ← required
    └── config_override.yaml  ← optional overlay (llm.* / chatcontext.*)
```

Currently committed:

- **Phase-1 (sub-agent dispatch)**: `simple/`, `dispatch/`,
  `accumulation/`, `compactor/`.
- **Phase-2 mission graph (spec 007)**:
  - `mission_graph/` — multi-step plan via `mission_plan`, status
    polling via `mission_status`, drain via `wait_for_missions`. Asserts
    spawn/result events on the coordinator + the `<system: missions
    complete>` marker after both children land.
  - `follow_up_routing/` — plan starts → user sends a refinement while
    the summariser is running → follow-up router classifies the message
    and routes it into the child's transcript (`user_followup_routed`
    audit row on the coordinator). `wait_for_missions_running` is the
    pre-step sentinel that ensures the router has a target.
  - `mission_cancel/` — plan starts → user asks to cancel the
    summariser → coordinator calls `mission_cancel`, both rows land
    `abandoned` (cascade). Verifies the meta-action carve-out: the
    cancel doesn't get rerouted into the child via follow-up routing.

Each scenario logs every GraphQL query result + dumps the final
`hub.db` path so DuckDB inspection is one `duckdb -readonly <path>`
away.

## Environment

1. Copy the template:
   ```bash
   cp tests/scenarios/.test.env.example tests/scenarios/.test.env
   ```
2. Fill `LLM_LOCAL_URL` + `EMBED_LOCAL_URL` (LM Studio or any
   OpenAI-compatible endpoint). Scenarios `t.Skip()` when either
   is missing, so unset = silent skip.
3. Optional knobs (see `.test.env.example`):
   - `INTEGRATION_AGENT_MODEL=<name>` — override the default model
     (must be registered in `tests/scenarios/config.yaml` under
     `local_db.models`).
   - `SCENARIO_PERSIST=/path/hub.db` — pin the artefact path
     instead of the default `.data/<name>-<ts>/memory.db`.
   - `DROP_DB=1` — remove the hub.db right after the scenario
     finishes (smoke-test mode, no artefact).
   - `SCENARIO_DEBUG=1` — crank scenario logs to DEBUG level.

## Running

```bash
make scenario                      # all scenarios
make scenario name=simple          # one — filters by subtest name
SCENARIO_PERSIST=/tmp/x.db \
  make scenario name=dispatch      # pin artefact path
DROP_DB=1 make scenario            # smoke: no leftovers
```

Behind `make scenario` is:

```bash
CGO_CFLAGS="-O1 -g" SCENARIO_NAME="$name" \
  go test -tags='duckdb_arrow scenario' -count=1 -v -timeout=600s \
  -run "TestScenarios" ./tests/scenarios/...
```

The `duckdb_arrow scenario` build tag pair hides scenarios from
`go test ./...` — they only compile under the explicit tag.

## scenario.yaml schema

```yaml
name: dispatch                        # optional, defaults to dir name
session_id: scenario-dispatch-1       # optional, defaults to "scenario-<name>-1"
config_override: config_override.yaml # optional, relative to scenario dir

steps:
  - say: "text of the user message"
    queries:
      - name: what_this_checks
        graphql: |
          query ($sid: String!) {
            hub { db { agent {
              sessions(filter: {id: {eq: $sid}}) { id session_type status }
            }}}
          }
        vars:
          sid: scenario-dispatch-1
        path: hub.db.agent.sessions   # optional — ScanDataJSON path
```

### `steps`

A list. Each step plays one user turn (`say`) and then runs zero
or more GraphQL `queries` before moving to the next step. The
classifier is drained between steps so queries see every async
event the turn produced.

### `queries`

Each entry is observational — no pass/fail contract, just the
response logged verbatim into `t.Log`. Use Hugr's top-level
[`jq()` query](https://hugr-lab.github.io/docs/5-graphql/4-jq-transformations)
for filter / group_by / reshape on the server so the log stays
focused:

```graphql
jq(query: ".hub.db.agent.sessions | map(select(.session_type == \"subagent\"))") {
  hub { db { agent { sessions { id session_type metadata } } } }
}
```

Result lands in `extensions.jq` on the response.

### Filter syntax

Follow [`skills/hugr-data/references/filter-guide.md`](../../skills/hugr-data/references/filter-guide.md):

- No `neq` / `not_eq` / `ne` — negation is `_not: {field: {eq: v}}`.
- Fields in `order_by` MUST also appear in the selection set.
- Logical ops: `_and` / `_or` take arrays; `_not` wraps a filter
  object.

### `config_override.yaml`

Only a small subset is honoured by `harness.applyConfigOverride`:

```yaml
llm:
  model: <str>                      # default intent model
  context_windows:
    <model_name>: <int>             # token budget per model
  default_budget: <int>             # fallback budget
  routes:
    <intent>: <model_name>

chatcontext:
  compaction_threshold: <float>     # 0.0–1.0, fraction of budget
```

Everything else (identity, embedder, providers, paths) comes from
`tests/scenarios/config.yaml` and is not scenario-overridable by
design — those are runtime-specific.

## Adding a scenario

1. `mkdir tests/scenarios/<name>/`
2. Write `scenario.yaml`. Start with `simple/scenario.yaml` and
   trim down.
3. (optional) Add `config_override.yaml` if the scenario needs a
   specific budget / threshold / model.
4. Run `make scenario name=<name>` — inspect the resulting
   `hub.db` under `tests/scenarios/.data/<name>-<ts>/memory.db`:
   ```bash
   duckdb -readonly tests/scenarios/.data/<name>-<ts>/memory.db \
     "SELECT seq, event_type, tool_name FROM session_events \
      WHERE session_id = 'scenario-<name>-1' ORDER BY seq"
   ```
5. Iterate on queries + prompts until the log + DB tell you what
   you wanted to see.

## Anti-patterns

- **Don't assert on LLM output.** LLMs are non-deterministic. If
  you catch yourself writing `assert.Equal(t, "exactly this",
  tr.FinalText)`, stop — either turn it into a soft log check
  (`t.Log(tr.FinalText)`) or move the contract to a unit test.
- **Don't share state across scenarios.** Each scenario gets its
  own hub.db; don't reach into another scenario's `.data/` dir.
- **Don't write scenarios that need two agents talking to each
  other.** Out of scope — Phase 2/3 territory.
- **Don't commit `.test.env`.** It holds URLs + sometimes API keys.
  The template (`*.example`) is the source of truth.
