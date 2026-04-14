# Query Engine Issues — discovered during hugen development

## ~~Issue 1: IPC transport does not deliver provider errors to client~~ — FIXED

---

## Issue 2: OpenAI provider uses `max_tokens` instead of `max_completion_tokens`

**Severity**: Medium — breaks newer OpenAI models (GPT-5, o-series)

**Files**: `pkg/data-sources/sources/llm/openai.go` lines 171, 367

**Problem**: Both `CreateChatCompletion` (line 171) and `CreateChatCompletionStream` (line 367) hardcode `"max_tokens"` in the request body:

```go
reqBody := map[string]any{
    "model":      strings.Trim(s.config.Model, "\""),
    "messages":   convertMessagesOpenAI(messages),
    "max_tokens": maxTokens,  // ← breaks newer models
}
```

Newer OpenAI models (GPT-5, o1, o3, etc.) require `max_completion_tokens` instead of `max_tokens`. Sending `max_tokens` returns HTTP 400:
```json
{
  "error": {
    "message": "Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead.",
    "type": "invalid_request_error",
    "param": "max_tokens",
    "code": "unsupported_parameter"
  }
}
```

**Options**:

A) **Always use `max_completion_tokens`** — backwards compatible with newer API versions, but may break very old models/endpoints:
```go
reqBody := map[string]any{
    "model":                strings.Trim(s.config.Model, "\""),
    "messages":             convertMessagesOpenAI(messages),
    "max_completion_tokens": maxTokens,
}
```

B) **Model-based detection** — use `max_completion_tokens` for known newer models:
```go
tokenKey := "max_tokens"
model := strings.Trim(s.config.Model, "\"")
if isNewModel(model) { // gpt-5*, o1*, o3*, chatgpt-5*, etc.
    tokenKey = "max_completion_tokens"
}
reqBody[tokenKey] = maxTokens
```

C) **Config flag** — add `use_max_completion_tokens: true` to data source config. Most flexible but requires user action.

D) **Try-and-fallback** — send `max_completion_tokens` first, if 400 with "unsupported_parameter" → retry with `max_tokens`. Robust but adds latency on first call for old models.

**Recommendation**: Option A — OpenAI API docs state `max_completion_tokens` is the preferred parameter for all current models. Legacy `max_tokens` is deprecated.

---

## ~~Issue 3: Gemini provider missing `thought_signature` in function calls~~ — FIXED

---

## ~~Issue 4: Gemma4 Jinja template crashes on missing `required` in tool parameters~~ — FIXED (workaround in hugen)

---

## Issue 5: LLMMessage needs `thinking` content support for Anthropic extended thinking

**Severity**: Low — future, not blocking now

**Problem**: `types.LLMMessage` has no way to represent "thinking" content blocks. Currently:
- **Gemini 2.5+**: Works via `ThoughtSignature` — the model reconstructs thinking from the signature, actual thought text can be skipped in message history
- **Anthropic**: Extended thinking requires `thinking` content blocks to be echoed back in subsequent turns. Without this, Anthropic models with extended thinking enabled will lose thinking context

**Current workaround in hugen**: Thought parts (`genai.Part.Thought == true`) are skipped in `contentToHugrMessages` — they're not sent to the provider at all. This works for Gemini but won't work for Anthropic.

**Proposed fix**: Add optional `thinking` field to `LLMMessage`:
```go
type LLMMessage struct {
    Role             string        `json:"role"`
    Content          string        `json:"content"`
    Thinking         string        `json:"thinking,omitempty"`         // Anthropic: thinking block content
    ToolCalls        []LLMToolCall `json:"tool_calls,omitempty"`
    ToolCallID       string        `json:"tool_call_id,omitempty"`
    ThoughtSignature string        `json:"thought_signature,omitempty"`
}
```

Provider-specific serialization:
- **Anthropic**: Emit as `{"type": "thinking", "thinking": "..."}` content block
- **Gemini**: Skip (use ThoughtSignature instead)
- **OpenAI**: Skip (no thinking support)
