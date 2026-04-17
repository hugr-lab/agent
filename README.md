# hugen

AI agent runtime for the [Hugr](https://hugr-lab.github.io) Data Mesh platform.

Built on [Google ADK Go](https://github.com/google/adk-go), uses Hugr as the LLM backend and data exploration layer via GraphQL and MCP.

## Operating Modes

- **Local / standalone** (`hugr_local.enabled: true`) — the agent boots an embedded Hugr engine in-process and attaches `data/memory.db` as the `hub.db` runtime source. LLM and embedding providers can be registered inside this local engine (`llm.mode: local`, `embedding.mode: local`) so the agent runs without any external Hugr instance.
- **Hub** (`hugr_local.enabled: false`) — the agent connects to a remote Hugr at `cfg.Hugr.URL` (or `memory.hugr_url` for a dedicated memory hub) and uses its catalog for both memory and model queries.

## Quick Start

```bash
# Configure connection
cp .env.example .env
# Edit .env with your Hugr server details

# Run A2A server (production default)
go run ./cmd/agent

# Run with dev web UI
go run ./cmd/agent devui

# Run in console mode
go run ./cmd/agent console
```

## Architecture

```
A2A / Dev UI / Console
        │
    HugrAgent (llmagent + custom wiring)
    ┌───┼───────────┐
    │   │            │
IntentLLM  DynamicToolset  PromptBuilder
(routes)   ┌────┴────┐    (constitution
    │      │         │     + skills)
HugrModel  System   MCP
(model.LLM) Tools   Tools
    │      │         │
    └──────┴────┬────┘
           Hugr Server
```

- **HugrAgent** wraps ADK's `llmagent` with dynamic prompt, dynamic tools, and intent-based LLM routing
- **IntentLLM Router** routes LLM calls by intent (default, tool_calling, summarization, classification)
- **DynamicToolset** manages runtime tool sets — system tools always loaded, MCP tools added on skill-load
- **PromptBuilder** assembles system prompt from constitution + skill catalog + active skill instructions
- **System Tools**: `skill_list`, `skill_load`, `skill_ref`, `context_status`
- **Skills** are on-disk packages (SKILL.md + references/ + mcp.yaml) loaded on demand

## Development

```bash
make build        # Build binary
make test         # Run tests with race detector
make check        # Vet + tests
make run          # Run A2A server
make run-devui    # Run with dev UI
make run-console  # Run in console mode
```

## Configuration

### Environment Variables (.env)

| Variable | Default | Description |
|----------|---------|-------------|
| `HUGR_URL` | `http://localhost:15000` | Hugr server URL |
| `HUGR_SECRET_KEY` | | Dev: static secret key auth |
| `HUGR_ACCESS_TOKEN` | | Production: token exchange initial token |
| `HUGR_TOKEN_URL` | | Production: token exchange service URL |
| `HUGR_OIDC_ISSUER` | | Dev: OIDC provider issuer URL |
| `HUGR_OIDC_CLIENT_ID` | | Dev: OIDC public client ID |
| `AGENT_MODEL` | `gemma4-26b` | Default Hugr LLM data source name |
| `AGENT_PORT` | `10000` | Server port |
| `AGENT_SKILLS_PATH` | `./skills` | Directory containing skill packages |
| `AGENT_BASE_URL` | `http://localhost:{port}` | Public base URL |
| `LOG_LEVEL` | `info` | Log level (info/debug) |

### YAML Configuration (config.yaml)

See `config.example.yaml` for the full annotated schema. Key sections:

- `agent` — identity (id, short_id, name, type)
- `hugr_local` — embedded engine toggle and DuckDB settings
- `memory` — `local|remote` mode + path for `data/memory.db`, volatility durations, scheduler cadence
- `llm` — routing (`default`, `tool_calling`, `summarization`, …) and mode
- `embedding` — model name + dimension (**frozen** at first run — see below)
- `models` — LLM/embedding provider URLs; format mirrors hugr's `core.data_sources` so the same source name works in local and hub modes

`llm.routes` and `skills.path` are hot-reloaded on edit.

### Data files (`data/`)

When `hugr_local.enabled: true`:

- `data/engine.db` — CoreDB file (hugr engine catalog, persisted schemas)
- `data/memory.db` — attached as `hub.db`, holds agent_types / agents / memory_items / sessions / …

Both are plain DuckDB files (gitignored). Deleting them restarts the agent from a clean slate. The files are flushed during graceful shutdown.

### HubDB

`interfaces.HubDB` is the unified Go API for the agent's data plane; the same implementation works over the embedded engine and over a remote Hugr client. In 004 scope:

- `AgentRegistry` — `GetAgentType`, `GetAgent`, `RegisterAgent`, `UpdateAgentActivity` (cross-agent `ListAgents` / `UpsertAgentType` are hub-only and stubbed)
- `Embeddings` — `Embed`, `EmbedBatch`, `Dimension`, `Available` via `core.models.embedding`; returns `hubdb.ErrEmbeddingDisabled` when no model is configured so callers can fall back to FTS
- `Memory`, `Learning`, `Sessions` — stubbed; filled in by spec 003b

### Embedding dimension is frozen

The embedding model and dimension are stored in the memory DB on first provision and verified on every subsequent run. Changing the model or dimension in config is fatal (would corrupt stored vectors) — the agent logs a clear error asking you to delete `data/memory.db` and re-create the agent.

## Project Structure

```
cmd/agent/              Entry point (A2A, devui, console modes) + engine/hubdb/sources wiring
interfaces/             Environment-agnostic contracts (HubDB, AgentRegistry, Memory, Learning, Sessions, Embeddings)
pkg/
  id/                   Synthetic sortable ID generator (agt_, mem_, sess_, hyp_, …)
  agent/                HugrAgent: agent wiring, DynamicToolset, PromptBuilder, TokenEstimator
  llms/intent/          Intent-based LLM routing
  tools/system/         Built-in tools: skill_list, skill_load, skill_ref, context_status
  models/hugr/          Hugr GraphQL LLM adapter (model.LLM)
  channels/             Streaming channel types for A2A SSE
  auth/                 Token stores (Remote, OIDC, secret key)
adapters/
  file/                 File-based SkillProvider and ConfigProvider
  hubdb/                HubDB over types.Querier; Source ("hub.db") + AgentRegistry + Embeddings
  hubdb/migrate/        DuckDB/Postgres provisioner for memory.db (schema + seed + embedding-version guard)
  test/                 Test adapters (ScriptedLLM, StaticSkillProvider)
internal/
  config/               YAML + .env loader
skills/                 Skill packages (SKILL.md + references/ + mcp.yaml)
constitution/           Base system prompt
```
