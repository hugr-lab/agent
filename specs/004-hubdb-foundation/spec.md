# Feature Specification: HubDB Foundation + Bootstrap

**Feature Branch**: `004-hubdb-foundation`
**Created**: 2026-04-17
**Status**: Draft
**Design**: `design/003-memory-storage/`

## User Scenarios & Testing

### User Story 1 — Agent starts with embedded Hugr engine (Priority: P1)

Agent process starts, creates an embedded query-engine instance with local DuckDB, attaches "hub.db" as a RuntimeSource module, deploys schema, and registers itself from config.

**Why this priority**: Nothing else works without the engine running. All memory, sessions, learning depend on this.

**Independent Test**: `go run ./cmd/agent` → logs show: engine initialized, hub.db attached, schema deployed, agent registered. `data/engine.db` and `data/memory.db` files created.

**Acceptance Scenarios**:

1. **Given** fresh install (no data/ directory), **When** agent starts, **Then** creates `data/engine.db` (CoreDB), `data/memory.db` (hub.db), deploys schema with version table, logs "hub.db attached"
2. **Given** existing `data/memory.db` with matching version, **When** agent starts, **Then** skips schema deployment, logs "schema up to date"
3. **Given** existing `data/memory.db` with different version, **When** agent starts and migration exists, **Then** runs migration, updates version table
4. **Given** `hugr_local.enabled: false` in config, **When** agent starts, **Then** skips local engine, only creates remote client (hub mode)

---

### User Story 2 — Agent type and instance registered from config (Priority: P1)

At startup, agent reads its identity from config.yaml and ensures agent_type + agent rows exist in hub.db. In hub mode this happens via remote Hugr.

**Why this priority**: agent_id is required for all DB operations (scoping). Without registration, nothing can be stored.

**Independent Test**: After startup, query `hub.db.agent_types` and `hub.db.agents` — rows exist with config values.

**Acceptance Scenarios**:

1. **Given** first startup, **When** no agent_type row exists, **Then** INSERT from config.yaml (id, name, description, config JSON)
2. **Given** agent_type already exists with same id, **When** agent starts, **Then** no duplicate (INSERT OR IGNORE)
3. **Given** agent instance row missing, **When** agent starts, **Then** INSERT agent with agent_type_id, short_id, name from config
4. **Given** agent already registered, **When** agent starts, **Then** updates last_active timestamp

---

### User Story 3 — LLM data sources registered in local engine (Priority: P1)

When `llm.mode == "local"`, agent registers LLM and embedding models from `config.yaml models[]` as data sources in the embedded engine. After startup, `core.models.chat_completion` subscriptions work through the local engine.

**Why this priority**: LLM calls are the agent's core function. Without models registered, agent can't think.

**Independent Test**: After startup with local LLM config, engine.Query for chat_completion works. Embedding model (if configured) responds to embedding mutation.

**Acceptance Scenarios**:

1. **Given** `llm.mode: "local"` and models[] configured, **When** agent starts, **Then** RegisterDataSource for each model, LoadDataSource succeeds
2. **Given** model with `api_key: ${GEMINI_API_KEY}`, **When** env var set, **Then** API key resolved in data source Path
3. **Given** embedding model configured, **When** agent starts, **Then** test embedding call succeeds, dimension matches config
4. **Given** embedding model configured but fails to load, **When** agent starts, **Then** logs warning, falls back to FTS (no vector search)
5. **Given** `llm.mode: "remote"`, **When** agent starts, **Then** no local data source registration, LLM goes through remote client

---

### User Story 4 — HubDB interface provides clean data access (Priority: P2)

HubDB wraps types.Querier and exposes Go methods for all agent data operations. Runtime never writes GraphQL directly. Same implementation works for both local and remote backends.

**Why this priority**: HubDB is the API surface for all subsequent features (memory, sessions, learning). Must be clean and tested.

**Independent Test**: Create HubDB with embedded engine, call AgentRegistry methods, verify data persists. Same test with remote client (if Hugr server available).

**Acceptance Scenarios**:

1. **Given** HubDB initialized with local engine, **When** call GetAgentType("hugr-data"), **Then** returns agent type with config from DB
2. **Given** HubDB initialized, **When** call RegisterAgent, then GetAgent, **Then** returns registered agent
3. **Given** HubDB initialized, **When** call ListAgents for a type, **Then** returns all agents of that type
4. **Given** HubDB initialized, **When** call UpdateAgentActivity, **Then** last_active timestamp updated
5. **Given** HubDB initialized, **When** call Close(), **Then** DuckDB WAL flushed, connections released

---

### User Story 5 — Synthetic sortable ID generation (Priority: P2)

All entities use synthetic IDs: `{prefix}_{agentShort}_{timestamp}_{random}`. IDs are time-sortable (string ORDER BY = chronological order), contain entity type prefix, and include agent identity.

**Why this priority**: ID format is used everywhere. Must be decided and implemented before any data storage.

**Independent Test**: Generate 1000 IDs — all unique, string-sorted = time-sorted, parseable for prefix and agent.

**Acceptance Scenarios**:

1. **Given** id.New("mem", "ag01"), **Then** format matches `mem_ag01_{10-digit-unix}_{6-hex-chars}`
2. **Given** two calls to id.New in sequence, **Then** second ID > first ID (string comparison)
3. **Given** 1000 concurrent calls, **Then** no duplicates (random suffix prevents collision)

---

### Edge Cases

- What happens when DuckDB file is corrupted? Agent logs fatal error, exits (not auto-delete)
- What happens when data/ directory is read-only? Fatal error at startup with clear message
- What happens when config.yaml is missing agent.id? Fatal error: "agent.id required"
- What happens when LLM model registration fails? Warn, continue if remote fallback available; fatal if local-only
- What happens when schema version table exists but is empty? Treat as uninitialized, apply schema

## Requirements

### Functional Requirements

- **FR-001**: System MUST embed query-engine as in-process Go library (no separate HTTP server)
- **FR-002**: System MUST attach memory DB as RuntimeSource named "hub.db" (AsModule=true)
- **FR-003**: Schema templates MUST support both DuckDB and PostgreSQL via Go template functions
- **FR-004**: System MUST use version table for schema migration tracking
- **FR-005**: System MUST seed agent_type and agent from config.yaml on first startup (idempotent)
- **FR-006**: All DB operations MUST be scoped to agent_id
- **FR-007**: IDs MUST be synthetic sortable format: `{prefix}_{agent}_{timestamp}_{random}`
- **FR-008**: HubDB interface MUST hide all GraphQL from the runtime
- **FR-009**: HubDB MUST work identically with local engine and remote client (types.Querier)
- **FR-010**: LLM data sources from config.yaml models[] MUST be registered in local engine when llm.mode == "local"
- **FR-011**: Embedding model availability MUST be verified at startup; fallback to FTS if unavailable
- **FR-012**: System MUST support soft delete on agent_types and agents tables
- **FR-013**: Schema MUST include TimescaleDB hypertable support (conditional on .IsTimescale template param)

### Key Entities

- **agent_types**: Configuration template (id, name, config JSON, soft delete)
- **agents**: Running instance (id, agent_type_id, short_id, name, status, soft delete)
- **version**: Schema version tracking (name, version, updated_at)
- All session/memory/learning tables created by schema but NOT implemented in this spec

## Success Criteria

- **SC-001**: Agent starts with embedded engine in under 5 seconds (cold start with schema init)
- **SC-002**: Agent starts with existing DB in under 2 seconds (warm start, no migration)
- **SC-003**: HubDB.AgentRegistry methods work for both local and remote (if Hugr available) backends
- **SC-004**: Schema templates render correctly for both DuckDB and PostgreSQL
- **SC-005**: `go test -race ./...` clean
- **SC-006**: Binary size acceptable (DuckDB adds ~50MB, expected)

## Assumptions

- query-engine v0.3.29+ is available and compatible
- duckdb-go/v2 CGO builds on target platform (darwin-arm64, linux-amd64)
- CoreDB from query-engine works with minimal config (no auth, no cache, no MCP)
- RuntimeSource pattern from CoreDB is stable API (not internal)
