package sessions

import (
	"testing"

	"github.com/hugr-lab/hugen/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/adk/model"
	adksession "google.golang.org/adk/session"
	"google.golang.org/genai"
)

func userText(t string) *adksession.Event {
	return &adksession.Event{
		Author: "user",
		LLMResponse: model.LLMResponse{
			Content: &genai.Content{Role: "user", Parts: []*genai.Part{{Text: t}}},
		},
	}
}

func agentText(t string) *adksession.Event {
	return &adksession.Event{
		Author: "hugr_agent",
		LLMResponse: model.LLMResponse{
			Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: t}}},
		},
	}
}

func TestClassify_UserMessage(t *testing.T) {
	rows := Classify(Envelope{SessionID: "s1", Event: userText("hi there")})
	require.Len(t, rows, 1)
	assert.Equal(t, store.EventTypeUserMessage, rows[0].EventType)
	assert.Equal(t, "user", rows[0].Author)
	assert.Equal(t, "hi there", rows[0].Content)
}

func TestClassify_AgentResponse(t *testing.T) {
	rows := Classify(Envelope{SessionID: "s1", Event: agentText("ok")})
	require.Len(t, rows, 1)
	assert.Equal(t, store.EventTypeLLMResponse, rows[0].EventType)
	assert.Equal(t, "ok", rows[0].Content)
}

func TestClassify_PartialDropped(t *testing.T) {
	ev := agentText("par")
	ev.Partial = true
	rows := Classify(Envelope{SessionID: "s1", Event: ev})
	assert.Empty(t, rows)
}

func TestClassify_InterruptedTruncated(t *testing.T) {
	ev := agentText("partial response...")
	ev.Interrupted = true
	rows := Classify(Envelope{SessionID: "s1", Event: ev})
	require.Len(t, rows, 1)
	assert.Equal(t, store.EventTypeLLMResponse, rows[0].EventType)
	require.NotNil(t, rows[0].Metadata)
	assert.Equal(t, true, rows[0].Metadata["truncated"])
}

func TestClassify_ToolCallAndResult(t *testing.T) {
	callEv := &adksession.Event{
		Author: "hugr_agent",
		LLMResponse: model.LLMResponse{
			Content: &genai.Content{Role: "model", Parts: []*genai.Part{{
				FunctionCall: &genai.FunctionCall{Name: "memory_search", Args: map[string]any{"query": "tf"}},
			}}},
		},
	}
	rows := Classify(Envelope{SessionID: "s1", Event: callEv})
	require.Len(t, rows, 1)
	assert.Equal(t, store.EventTypeToolCall, rows[0].EventType)
	assert.Equal(t, "memory_search", rows[0].ToolName)
	assert.Equal(t, "tf", rows[0].ToolArgs["query"])

	respEv := &adksession.Event{
		Author: "tool",
		LLMResponse: model.LLMResponse{
			Content: &genai.Content{Role: "user", Parts: []*genai.Part{{
				FunctionResponse: &genai.FunctionResponse{Name: "memory_search", Response: map[string]any{"results": []int{1, 2, 3}}},
			}}},
		},
	}
	rows = Classify(Envelope{SessionID: "s1", Event: respEv})
	require.Len(t, rows, 1)
	assert.Equal(t, store.EventTypeToolResult, rows[0].EventType)
	assert.Contains(t, rows[0].ToolResult, "results")
}

func TestClassify_ToolResultTruncated(t *testing.T) {
	big := make([]byte, maxToolResultBytes+100)
	for i := range big {
		big[i] = 'x'
	}
	ev := &adksession.Event{
		Author: "tool",
		LLMResponse: model.LLMResponse{
			Content: &genai.Content{Parts: []*genai.Part{{
				FunctionResponse: &genai.FunctionResponse{Name: "big_tool", Response: map[string]any{"blob": string(big)}},
			}}},
		},
	}
	rows := Classify(Envelope{SessionID: "s1", Event: ev})
	require.Len(t, rows, 1)
	assert.LessOrEqual(t, len(rows[0].ToolResult), maxToolResultBytes+32)
	assert.Equal(t, true, rows[0].Metadata["truncated"])
}

func TestPublish_DropOnFull(t *testing.T) {
	// tiny buffer, fill it, third publish should drop.
	c := NewClassifier(nil, nil, 1)
	ok := c.Publish(Envelope{SessionID: "s1", Event: userText("a")})
	require.True(t, ok)
	ok = c.Publish(Envelope{SessionID: "s1", Event: userText("b")})
	assert.False(t, ok)
	assert.Equal(t, int64(1), c.Dropped())
}
