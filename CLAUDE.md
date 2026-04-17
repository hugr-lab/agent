# agent Development Guidelines

Auto-generated from all feature plans. Last updated: 2026-04-17

## Active Technologies
- Go 1.26.1 + ADK Go v1.1.0 (`agent.New`, `runner.Runner`, `adka2a.Executor`, `mcptoolset`, `tool.Toolset`), a2a-go v0.3.13, MCP Go SDK v1.4.1, hugr query-engine/client v0.3.28, viper v1.21 (002-runtime-bootstrap)
- File-based (skills on disk, YAML config, .env secrets) (002-runtime-bootstrap)
- DuckDB (embedded, file-based) via query-engine. CoreDB `data/engine.db` + memory `data/memory.db`. (004-hubdb-foundation)

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
- 004-hubdb-foundation: Embedded hugr engine (local mode) with data/engine.db (CoreDB) + data/memory.db attached as "hub.db" RuntimeSource. adapters/hubdb/ exposes HubDB over types.Querier (works for embedded engine + remote client). adapters/hubdb/migrate provisions and migrates the memory DB with a frozen embedding model+dim guard. GraphQL compiler Prefix is underscore-mapped ("hub_db_*") so typed variable declarations parse; query-path hierarchy stays "hub { db { ... } }". setupLocalSources registers llm-*/embedding data sources in the embedded engine with a single probe against core.models.embedding to verify dimension.
- 002-runtime-bootstrap: Added Go 1.26.1 + ADK Go v1.1.0 (`agent.New`, `runner.Runner`, `adka2a.Executor`, `mcptoolset`, `tool.Toolset`), a2a-go v0.3.13, MCP Go SDK v1.4.1, hugr query-engine/client v0.3.28, viper v1.21

- 001-agent-prototype: Added Go 1.26.1 + ADK Go (`google.golang.org/adk`), Hugr Go client (`github.com/hugr-lab/query-engine/client`), Viper (`github.com/spf13/viper`)

## Build & test

Always build and test with the DuckDB Arrow tag so `duckdb-go/v2` exposes Arrow paths:

```bash
CGO_CFLAGS="-O1 -g" go build -tags=duckdb_arrow ./...
CGO_CFLAGS="-O1 -g" go test  -tags=duckdb_arrow ./...
```

The Makefile's `build`, `build-debug`, `test`, `check` targets set these flags for you.

<!-- MANUAL ADDITIONS START -->
<!-- MANUAL ADDITIONS END -->
