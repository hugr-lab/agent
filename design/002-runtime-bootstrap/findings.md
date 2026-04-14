# Runtime Bootstrap — Implementation Findings

Findings discovered during Phase 2 (002-runtime-bootstrap) implementation that inform future design decisions.

## F1: Hugr Rejects `"parameters": null` in Tool Declarations

**Problem**: When a tool has no parameters (e.g. `skill_list`, `context_status`), the `genai.FunctionDeclaration` has `Parameters: nil`. This serializes to `"parameters": null` in JSON. Hugr (and OpenAI API format) silently rejects the entire request — the subscription closes immediately with no error, returning an empty finish event (`model=""`, `prompt_tokens=0`).

**Fix**: Default null parameters to `{"type": "object", "properties": {}, "required": []}` in `adkToHugrTools()`.

**Lesson**: Hugr does not return errors for malformed tool declarations — it silently drops the request. We should validate tool format before sending to catch this early.

## F2: ADK `genai.Schema` Uses UPPERCASE Type Names

**Problem**: ADK's `genai.Schema` uses `Type: "OBJECT"`, `Type: "STRING"` (Go const values). Hugr/OpenAI format expects lowercase `"object"`, `"string"`. Sending UPPERCASE types causes Hugr to reject tools silently (same as F1).

**Fix**: Added `schemaToMap()` that converts `genai.Schema` to a raw map with `strings.ToLower(Type)`. MCP tools don't have this issue because they use `ParametersJsonSchema` (raw JSON, already lowercase).

**Lesson**: Any tool created via ADK's `genai.FunctionDeclaration` with `Parameters: &genai.Schema{...}` needs this normalization. MCP tools bypass it via `ParametersJsonSchema`.

## F3: Constitution Tool Names Must Match Exactly

**Problem**: Constitution used `skill-list` / `skill-load` / `skill-ref` (dashes) but tools were registered as `skill_list` / `skill_load` / `skill_ref` (underscores). The model couldn't map the instructions to the available tools and never called them.

**Fix**: Updated constitution to use exact tool names with underscores.

**Lesson**: Tool names in system prompt must exactly match the `Name()` returned by the tool. Use underscores (not dashes) since ADK/OpenAI function names follow identifier conventions.

## F4: Custom agent.New() Requires Internal ADK APIs for Tool Execution

**Problem**: The original plan called for `agent.New(Config{Run: ...})` with a custom Run function. However, executing tools from a custom Run function requires:
- `toolinternal.FunctionTool` interface (internal)
- `toolinternal.NewToolContext()` (internal)
- `toolinternal.RequestProcessor` (internal)

These are not importable from external modules.

**Decision**: Use `llmagent.New()` with callbacks instead. This provides:
- `InstructionProvider` → dynamic prompts (PromptBuilder)
- `Toolsets` → dynamic tools (DynamicToolset)
- `AfterModelCallbacks` → token calibration
- Full tool execution lifecycle for free

**Trade-off**: Less control over streaming events (can't inject custom channel metadata at the Event level). Acceptable for Phase 2; revisit if needed.

## F5: MCP Skill Configuration Needs Registry Architecture

**Current state**: Each skill has a single MCP endpoint in `mcp.yaml`:
```yaml
endpoint: ${HUGR_MCP_URL}
```

**Requirement** (from user feedback): Skills should reference named MCP servers from a central registry, with tool filtering:

```yaml
# skills/{name}/mcp.yaml
servers:
  # Named server from central registry (all tools):
  - name: hugr

  # Named server, specific tools only:
  - name: analytics
    tools: [run_query, get_schema]

  # Inline server (configured in skill):
  - name: local-processor
    endpoint: ${PROCESSOR_MCP_URL}
    tools: [process, validate]
```

**Design implications**:
1. **MCP Server Registry** — central store of named servers with endpoints/transports
2. **Tool filtering** — `DynamicToolset` needs per-server tool whitelisting (ADK has `tool.FilterToolset` for this)
3. **Multi-server skills** — a single skill can mount tools from multiple MCP servers
4. **Inline servers** — create ephemeral registry entries as `{skill-name}/{server-name}`
5. **Server lifecycle** — connect lazily on first `skill_load`, disconnect on skill unload

**Next step**: Design document for MCP Registry (design/003-*).

## F6: ADK Flow Caches Tools Per Invocation

**Problem**: ADK's internal `Flow.toolProcessor` resolves tools once and caches them in `f.Tools`. Adding tools to `DynamicToolset` during `skill_load` (mid-invocation) has no effect — the Flow already has the old tool list.

```go
// base_flow.go, toolProcessor:
if f.Tools != nil {
    return  // CACHED — skip re-resolution
}
```

**Impact**: MCP tools added via `skill_load` are invisible until the next invocation (next user message).

**Workaround**: Pre-load MCP tools at startup so they're available from the first invocation. Skill system manages instructions/references only, not MCP connections.

**Future**: Either fork the ADK tool processor to invalidate cache, or restructure the flow so `skill_load` triggers a new invocation.

## F7: Anthropic Requires Dict for tool_use Input, Never Null

**Problem**: When a tool like `skill_list` has no arguments, ADK sets `FunctionCall.Args = nil`. Our `contentToHugrMessages` serializes this as `"arguments": null`. Anthropic's API requires `input` to be a valid dictionary, returning HTTP 400: `"tool_use.input: Input should be a valid dictionary"`.

**Fix**: Default `nil` Args to `map[string]any{}` in `contentToHugrMessages`.

## F8: Gemma4 Poor Tool Calling Support (Informational)

**Problem**: Gemma4-26b with `tool_choice: "auto"` ignores tools entirely and responds with text. With `tool_choice: "required"` it calls tools but generates excessive reasoning tokens (15K+) and hallucinates tool names (`project_context_context_status______________________/usrly-data-mesh-st/`).

**Decision**: Gemma4 is not suitable for tool-calling agents. Use Claude Sonnet, GPT-4, or Gemini Pro for production.

## F9: Sessions Must Be Fully Independent (Hard Requirement) (Hard Requirement)

**Problem**: The original prototype used singleton `PromptBuilder` and `DynamicToolset` shared across all sessions. When one session loaded a skill, all other sessions saw it. The `sessionTracker` (BeforeModelCallback) tried to fix this by resetting state on session change, but with parallel sessions it caused ping-pong resets — every LLM call cleared the previous session's state.

**Symptoms**:
- Skills "not loading" — session A loads a skill, session B's model call resets it
- MCP tools visible before `skill_load` — pre-loaded globally at startup
- Tool/prompt state leaking between concurrent users

**Fix** (implemented):
1. `PromptBuilder` — per-session state map (`skillInstr`, `catalog`, `extras`, `activeSkill`) with `defaultCatalog` fallback for new sessions
2. `DynamicToolset` — global children (system tools) + `sessionChildren` map for per-session MCP tools
3. MCP tools added to session only when `skill_load` is called, not at startup
4. Removed `sessionTracker` entirely — session isolation is structural, not callback-based
5. All system tools use `ctx.SessionID()` for state mutation

**Invariant**: Any mutable state that depends on user interaction (loaded skill, prompt additions, MCP tools) MUST be keyed by session ID. Global state is only for immutable/startup data (constitution, system tools, pre-created MCP connections).

