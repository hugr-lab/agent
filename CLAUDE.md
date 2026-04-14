# agent Development Guidelines

Auto-generated from all feature plans. Last updated: 2026-04-14

## Active Technologies
- Go 1.26.1 + ADK Go v1.1.0 (`agent.New`, `runner.Runner`, `adka2a.Executor`, `mcptoolset`, `tool.Toolset`), a2a-go v0.3.13, MCP Go SDK v1.4.1, hugr query-engine/client v0.3.28, viper v1.21 (002-runtime-bootstrap)
- File-based (skills on disk, YAML config, .env secrets) (002-runtime-bootstrap)

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
- 002-runtime-bootstrap: Added Go 1.26.1 + ADK Go v1.1.0 (`agent.New`, `runner.Runner`, `adka2a.Executor`, `mcptoolset`, `tool.Toolset`), a2a-go v0.3.13, MCP Go SDK v1.4.1, hugr query-engine/client v0.3.28, viper v1.21

- 001-agent-prototype: Added Go 1.26.1 + ADK Go (`google.golang.org/adk`), Hugr Go client (`github.com/hugr-lab/query-engine/client`), Viper (`github.com/spf13/viper`)

<!-- MANUAL ADDITIONS START -->
<!-- MANUAL ADDITIONS END -->
