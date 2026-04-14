# hugr-agent

AI agent runtime for the [Hugr](https://hugr-lab.github.io) Data Mesh platform.

Built on [Google ADK Go](https://github.com/google/adk-go), uses Hugr as the LLM backend and data exploration layer via GraphQL and MCP.

## Quick Start

```bash
# Configure connection
cp .env.example .env
# Edit .env with your Hugr server details

# Run with web UI
go run ./cmd/agent web
# Open http://localhost:8000

# Run in console mode
go run ./cmd/agent
```

## Architecture

```
ADK Launcher (console / web UI / API)
        │
    llmagent.Agent
    ┌───┴───┐
    │       │
HugrModel  mcptoolset
(model.LLM) (Hugr /mcp)
    │       │
    └───┬───┘
   Hugr Server
```

- **HugrModel** implements ADK's `model.LLM` interface, routing LLM calls through Hugr's `core.models.chat_completion` GraphQL API.
- **MCP Toolset** connects to Hugr's `/mcp` endpoint, exposing 10 data exploration tools (discovery, schema, query execution).

## Development

```bash
make build    # Build binary
make test     # Run tests
make check    # Vet + tests
make run-web  # Run with web UI
```

## Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `HUGR_URL` | `http://localhost:15000` | Hugr server URL |
| `HUGR_SECRET_KEY` | (required) | Authentication key |
| `AGENT_MODEL` | `gemma4-26b` | Hugr LLM data source name |
