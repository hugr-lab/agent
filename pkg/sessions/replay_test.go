package sessions

import (
	"testing"

	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReplaySkillState_LoadUnloadSequence(t *testing.T) {
	events := []sessstore.Event{
		{EventType: sessstore.EventTypeSkillLoaded, Content: "hugr-data"},
		{EventType: sessstore.EventTypeSkillUnloaded, Content: "hugr-data"},
		{EventType: sessstore.EventTypeSkillLoaded, Content: "hugr-data"},
		{EventType: sessstore.EventTypeSkillLoaded, Content: "weather"},
		{EventType: sessstore.EventTypeSkillUnloaded, Content: "weather"},
		{EventType: sessstore.EventTypeSkillLoaded, Content: "weather"},
	}
	state := replaySkillState(events)
	assert.Contains(t, state, "hugr-data")
	assert.Contains(t, state, "weather")
	assert.Len(t, state, 2)
}

func TestReplaySkillState_MetadataFallback(t *testing.T) {
	// Legacy rows may have name in metadata.skill instead of Content.
	events := []sessstore.Event{
		{EventType: sessstore.EventTypeSkillLoaded, Metadata: map[string]any{"skill": "legacy"}},
	}
	state := replaySkillState(events)
	assert.Contains(t, state, "legacy")
}

func TestReplaySkillState_ConversationEventsIgnored(t *testing.T) {
	events := []sessstore.Event{
		{EventType: sessstore.EventTypeUserMessage, Content: "hello"},
		{EventType: sessstore.EventTypeLLMResponse, Content: "hi"},
		{EventType: sessstore.EventTypeToolCall, ToolName: "x"},
	}
	assert.Empty(t, replaySkillState(events))
}

func TestConvertToADKEvent_UserMessage(t *testing.T) {
	adk, ok := convertToADKEvent(sessstore.Event{
		ID:        "ev1",
		EventType: sessstore.EventTypeUserMessage,
		Author:    "user-1",
		Content:   "привет",
	})
	require.True(t, ok)
	require.NotNil(t, adk.Content)
	assert.Equal(t, "user", adk.Content.Role)
	require.Len(t, adk.Content.Parts, 1)
	assert.Equal(t, "привет", adk.Content.Parts[0].Text)
	assert.True(t, adk.TurnComplete)
}

func TestConvertToADKEvent_LLMResponseWithUsage(t *testing.T) {
	adk, ok := convertToADKEvent(sessstore.Event{
		ID:        "ev2",
		EventType: sessstore.EventTypeLLMResponse,
		Author:    "model",
		Content:   "ответ",
		Metadata: map[string]any{
			"model":             "gemini-pro-3-1",
			"prompt_tokens":     42,
			"completion_tokens": 7,
		},
	})
	require.True(t, ok)
	assert.Equal(t, "model", adk.Content.Role)
	assert.Equal(t, "ответ", adk.Content.Parts[0].Text)
	assert.Equal(t, "gemini-pro-3-1", adk.ModelVersion)
	require.NotNil(t, adk.UsageMetadata)
	assert.EqualValues(t, 42, adk.UsageMetadata.PromptTokenCount)
	assert.EqualValues(t, 7, adk.UsageMetadata.CandidatesTokenCount)
	assert.EqualValues(t, 49, adk.UsageMetadata.TotalTokenCount)
}

func TestConvertToADKEvent_ToolCall(t *testing.T) {
	adk, ok := convertToADKEvent(sessstore.Event{
		ID:        "ev3",
		EventType: sessstore.EventTypeToolCall,
		ToolName:  "data-query",
		ToolArgs:  map[string]any{"limit": 10},
	})
	require.True(t, ok)
	require.Len(t, adk.Content.Parts, 1)
	fc := adk.Content.Parts[0].FunctionCall
	require.NotNil(t, fc)
	assert.Equal(t, "data-query", fc.Name)
	assert.Equal(t, 10, fc.Args["limit"])
}

func TestConvertToADKEvent_ToolResultStructured(t *testing.T) {
	adk, ok := convertToADKEvent(sessstore.Event{
		ID:         "ev4",
		EventType:  sessstore.EventTypeToolResult,
		ToolName:   "data-query",
		ToolResult: `{"rows":42,"ok":true}`,
	})
	require.True(t, ok)
	fr := adk.Content.Parts[0].FunctionResponse
	require.NotNil(t, fr)
	assert.Equal(t, "data-query", fr.Name)
	assert.EqualValues(t, 42, fr.Response["rows"])
	assert.Equal(t, true, fr.Response["ok"])
}

func TestConvertToADKEvent_ToolResultNonObject(t *testing.T) {
	adk, ok := convertToADKEvent(sessstore.Event{
		ID:         "ev5",
		EventType:  sessstore.EventTypeToolResult,
		ToolName:   "probe",
		ToolResult: `"just a string"`,
	})
	require.True(t, ok)
	fr := adk.Content.Parts[0].FunctionResponse
	require.NotNil(t, fr)
	assert.Equal(t, "just a string", fr.Response["result"])
}

func TestConvertToADKEvent_NonConversationSkipped(t *testing.T) {
	for _, ev := range []sessstore.Event{
		{EventType: sessstore.EventTypeSkillLoaded, Content: "x"},
		{EventType: sessstore.EventTypeSkillUnloaded, Content: "x"},
		{EventType: sessstore.EventTypeNote, Content: "note"},
		{EventType: sessstore.EventTypeError, Content: "oops"},
		{EventType: sessstore.EventTypeSessionForked, Content: "forked"},
	} {
		_, ok := convertToADKEvent(ev)
		assert.False(t, ok, "event type %q should not yield adksession.Event", ev.EventType)
	}
}
