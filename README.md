# hugr-agent

AI agent runtime for the [Hugr](https://hugr-lab.github.io) Data Mesh platform.

Built on [Google ADK Go](https://github.com/google/adk-go), uses Hugr as the LLM backend and data exploration layer via GraphQL and MCP.

## Quick Start

```bash
# Configure connection
cp .env.example .env
# Edit .env with your Hugr server details

# Run A2A server (production default)
go run ./cmd/hugr-agent

# Run with dev web UI
go run ./cmd/hugr-agent devui

# Run in console mode
go run ./cmd/hugr-agent console
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

```yaml
llm:
  routes:
    default: gemma4-26b
    # tool_calling: gemma-local
    # summarization: gemma-tiny

skills:
  path: ./skills
```

Config changes are hot-reloaded automatically.

## Project Structure

```
cmd/hugr-agent/         Entry point (A2A, devui, console modes)
interfaces/             Environment-agnostic contracts
pkg/
  hugragent/            HugrAgent: agent wiring, DynamicToolset, PromptBuilder, TokenEstimator
  intentllm/            Intent-based LLM routing
  systemtools/          Built-in tools: skill_list, skill_load, skill_ref, context_status
  hugrmodel/            Hugr GraphQL LLM adapter (model.LLM)
  channels/             Streaming channel types for A2A SSE
  auth/                 Token stores (Remote, OIDC, secret key)
adapters/
  file/                 File-based SkillProvider and ConfigProvider
  test/                 Test adapters (ScriptedLLM, StaticSkillProvider)
skills/                 Skill packages (SKILL.md + references/ + mcp.yaml)
constitution/           Base system prompt
```
