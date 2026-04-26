# agent Development Guidelines

Auto-generated from all feature plans. Last updated: 2026-04-26

## Active Technologies
- Go 1.26.1 + ADK Go v1.1.0 (`agent.New`, `runner.Runner`, `adka2a.Executor`, `mcptoolset`, `tool.Toolset`), a2a-go v0.3.13, MCP Go SDK v1.4.1, hugr query-engine/client v0.3.28, viper v1.21 (002-runtime-bootstrap)
- File-based (skills on disk, YAML config, .env secrets) (002-runtime-bootstrap)
- DuckDB (embedded, file-based) via query-engine. CoreDB `data/engine.db` + memory `data/memory.db`. (004-hubdb-foundation)
- Go 1.26.1 + `google.golang.org/adk` (v1.1.0), `github.com/hugr-lab/query-engine` (embedded mode + client v0.3.28), `github.com/marcboeker/go-duckdb/v2` (via query-engine; `duckdb_arrow` build tag required), `log/slog`, `context`, `sync`. No new top-level deps introduced by this spec. (005-memory-learning)
- Existing `hub.db` DuckDB file (`data/memory.db` attached as `hub.db` RuntimeSource). Schema from 004 covers all tables this spec uses. (005-memory-learning)
- Embedded hugr engine against DuckDB `data/memory.db` attached as `hub.db` runtime source. Remote mode (PostgreSQL-backed hub) uses the same `types.Querier` and same GraphQL schema. All schema edits go through the template-driven migration in `pkg/store/local/migrate/`. (006-agent-loop-foundation)
- Go 1.26.1 (CGO_CFLAGS="-O1 -g"; `duckdb_arrow` build tag mandatory for engine paths). + `google.golang.org/adk` v1.1.0 (agent, runner, tool, model, session), `github.com/hugr-lab/query-engine` (embedded + client v0.3.28+ with JSON-literal fix from `c2ce14a`), `github.com/marcboeker/go-duckdb/v2`, `log/slog`, `context`, `sync`, `sync/atomic`, `crypto/sha256` (idempotency cache key). No new top-level deps. (007-missions-async)
- DuckDB `data/memory.db` attached as `hub.db` via local engine (spec 004); additive migration `0.0.3` adds `mission_deps` table + three `EventType*` constants. Remote mode (Postgres-backed hub) uses the same GraphQL schema; all schema edits go through the template-driven migration path in `pkg/store/local/migrate/`. (007-missions-async)
- Go 1.26.1 (CGO_CFLAGS="-O1 -g"; `duckdb_arrow` build tag mandatory for engine paths). + `google.golang.org/adk` v1.1.0 (artifact, tool, model, session), `github.com/hugr-lab/query-engine` (embedded + client v0.3.28+), `github.com/marcboeker/go-duckdb/v2`, `github.com/spf13/viper`, `log/slog`, `context`, `sync`, `crypto/sha256` (id derivation), `crypto/rand` + `encoding/hex` (id randomness), `mime` + `net/http` (download endpoint). No new top-level deps. The S3 stub does **not** import an AWS SDK. (008-artifact-registry)
- DuckDB `data/memory.db` attached as `hub.db` via local engine (spec 004); additive migration `0.0.3` adds `artifacts` and `artifact_grants` tables plus the `session_artifacts` recursive view. Remote mode (Postgres-backed hub) uses the same GraphQL schema; secondary indexes are gated on `{{ if isPostgres }}` — DuckDB stays index-free for these tables (project rule). The artifacts' bytes do **not** live in `hub.db`: they live in the active `Storage` backend selected by config (`fs` writes under `cfg.Artifacts.FS.Dir`). (008-artifact-registry)
- Go 1.26.1 (CGO_CFLAGS="-O1 -g"; `duckdb_arrow` build tag mandatory for engine paths). + `google.golang.org/adk` v1.1.0 (tool, model, session, event), `github.com/hugr-lab/query-engine` (embedded + client v0.3.28+), `github.com/marcboeker/go-duckdb/v2`, `github.com/spf13/viper`, `log/slog`, `context`, `sync`, `sync/atomic` (policy cache version), `time`. No new top-level dependencies. Hugr's `semantic:` argument on `session_events` (added in phase 1) is the only "new" interface this spec leans on, and it's already in `query-engine`. (009-hitl-search-composition)
- DuckDB `data/memory.db` attached as `hub.db` via local engine (spec 004); additive migration `0.0.5` adds two tables — `approvals` and `tool_policies`. Bytes never enter `session_events` (gated tool calls store `args` JSON inline on the approvals row, not in the events stream beyond the lifecycle metadata blob). Remote mode (Postgres-backed hub) uses the same GraphQL schema; **all secondary indexes (composite + per-scope) are gated on `{{ if isPostgres }}` — DuckDB stays index-free for both new tables**, consistent with the project rule established in phases 1-3. (009-hitl-search-composition)

- Go 1.26.1 + ADK Go (`google.golang.org/adk`), Hugr Go client (`github.com/hugr-lab/query-engine/client`), Viper (`github.com/spf13/viper`) (001-agent-prototype)

## Project Structure

```text
backend/
frontend/
tests/
```

## Commands

# Add commands for Go 1.26.1

## Code Style

Go 1.26.1: Follow standard conventions

## Recent Changes
- 009-hitl-search-composition: Added Go 1.26.1 (CGO_CFLAGS="-O1 -g"; `duckdb_arrow` build tag mandatory for engine paths). + `google.golang.org/adk` v1.1.0 (tool, model, session, event), `github.com/hugr-lab/query-engine` (embedded + client v0.3.28+), `github.com/marcboeker/go-duckdb/v2`, `github.com/spf13/viper`, `log/slog`, `context`, `sync`, `sync/atomic` (policy cache version), `time`. No new top-level dependencies. Hugr's `semantic:` argument on `session_events` (added in phase 1) is the only "new" interface this spec leans on, and it's already in `query-engine`.
- 008-artifact-registry: Added Go 1.26.1 (CGO_CFLAGS="-O1 -g"; `duckdb_arrow` build tag mandatory for engine paths). + `google.golang.org/adk` v1.1.0 (artifact, tool, model, session), `github.com/hugr-lab/query-engine` (embedded + client v0.3.28+), `github.com/marcboeker/go-duckdb/v2`, `github.com/spf13/viper`, `log/slog`, `context`, `sync`, `crypto/sha256` (id derivation), `crypto/rand` + `encoding/hex` (id randomness), `mime` + `net/http` (download endpoint). No new top-level deps. The S3 stub does **not** import an AWS SDK.
- 007-missions-async: Added Go 1.26.1 (CGO_CFLAGS="-O1 -g"; `duckdb_arrow` build tag mandatory for engine paths). + `google.golang.org/adk` v1.1.0 (agent, runner, tool, model, session), `github.com/hugr-lab/query-engine` (embedded + client v0.3.28+ with JSON-literal fix from `c2ce14a`), `github.com/marcboeker/go-duckdb/v2`, `log/slog`, `context`, `sync`, `sync/atomic`, `crypto/sha256` (idempotency cache key). No new top-level deps.


## Build & test

Always build and test with the DuckDB Arrow tag so `duckdb-go/v2` exposes Arrow paths:

```bash
CGO_CFLAGS="-O1 -g" go build -tags=duckdb_arrow ./...
CGO_CFLAGS="-O1 -g" go test  -tags=duckdb_arrow ./...
```

The Makefile's `build`, `build-debug`, `test`, `check` targets set these flags for you.

<!-- MANUAL ADDITIONS START -->
<!-- MANUAL ADDITIONS END -->
