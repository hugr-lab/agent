# agent Development Guidelines

Auto-generated from all feature plans. Last updated: 2026-04-24

## Active Technologies
- Go 1.26.1 + ADK Go v1.1.0 (`agent.New`, `runner.Runner`, `adka2a.Executor`, `mcptoolset`, `tool.Toolset`), a2a-go v0.3.13, MCP Go SDK v1.4.1, hugr query-engine/client v0.3.28, viper v1.21 (002-runtime-bootstrap)
- File-based (skills on disk, YAML config, .env secrets) (002-runtime-bootstrap)
- DuckDB (embedded, file-based) via query-engine. CoreDB `data/engine.db` + memory `data/memory.db`. (004-hubdb-foundation)
- Go 1.26.1 + `google.golang.org/adk` (v1.1.0), `github.com/hugr-lab/query-engine` (embedded mode + client v0.3.28), `github.com/marcboeker/go-duckdb/v2` (via query-engine; `duckdb_arrow` build tag required), `log/slog`, `context`, `sync`. No new top-level deps introduced by this spec. (005-memory-learning)
- Existing `hub.db` DuckDB file (`data/memory.db` attached as `hub.db` RuntimeSource). Schema from 004 covers all tables this spec uses. (005-memory-learning)
- Embedded hugr engine against DuckDB `data/memory.db` attached as `hub.db` runtime source. Remote mode (PostgreSQL-backed hub) uses the same `types.Querier` and same GraphQL schema. All schema edits go through the template-driven migration in `pkg/store/local/migrate/`. (006-agent-loop-foundation)
- Go 1.26.1 (CGO_CFLAGS="-O1 -g"; `duckdb_arrow` build tag mandatory for engine paths). + `google.golang.org/adk` v1.1.0 (agent, runner, tool, model, session), `github.com/hugr-lab/query-engine` (embedded + client v0.3.28+ with JSON-literal fix from `c2ce14a`), `github.com/marcboeker/go-duckdb/v2`, `log/slog`, `context`, `sync`, `sync/atomic`, `crypto/sha256` (idempotency cache key). No new top-level deps. (007-missions-async)
- DuckDB `data/memory.db` attached as `hub.db` via local engine (spec 004); additive migration `0.0.3` adds `mission_deps` table + three `EventType*` constants. Remote mode (Postgres-backed hub) uses the same GraphQL schema; all schema edits go through the template-driven migration path in `pkg/store/local/migrate/`. (007-missions-async)

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
- 007-missions-async: Added Go 1.26.1 (CGO_CFLAGS="-O1 -g"; `duckdb_arrow` build tag mandatory for engine paths). + `google.golang.org/adk` v1.1.0 (agent, runner, tool, model, session), `github.com/hugr-lab/query-engine` (embedded + client v0.3.28+ with JSON-literal fix from `c2ce14a`), `github.com/marcboeker/go-duckdb/v2`, `log/slog`, `context`, `sync`, `sync/atomic`, `crypto/sha256` (idempotency cache key). No new top-level deps.
- 006-agent-loop-foundation: Added Go 1.26.1
- 005-memory-learning: Added Go 1.26.1 + `google.golang.org/adk` (v1.1.0), `github.com/hugr-lab/query-engine` (embedded mode + client v0.3.28), `github.com/marcboeker/go-duckdb/v2` (via query-engine; `duckdb_arrow` build tag required), `log/slog`, `context`, `sync`. No new top-level deps introduced by this spec.


## Build & test

Always build and test with the DuckDB Arrow tag so `duckdb-go/v2` exposes Arrow paths:

```bash
CGO_CFLAGS="-O1 -g" go build -tags=duckdb_arrow ./...
CGO_CFLAGS="-O1 -g" go test  -tags=duckdb_arrow ./...
```

The Makefile's `build`, `build-debug`, `test`, `check` targets set these flags for you.

<!-- MANUAL ADDITIONS START -->
<!-- MANUAL ADDITIONS END -->
