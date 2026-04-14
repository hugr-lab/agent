// Package channels defines the channel protocol for streaming agent reasoning steps.
package channels

import (
	"google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// Channel classifies streamed events for clients.
const (
	ChannelStatus       = "status"
	ChannelToolCall     = "tool_call"
	ChannelToolError    = "tool_error"
	ChannelContentDelta = "content_delta"
	ChannelFinal        = "final"
)

// EmitStatus creates a partial event with a status message.
func EmitStatus(invocationID, agentName, text string) *session.Event {
	e := session.NewEvent(invocationID)
	e.Author = agentName
	e.Content = &genai.Content{
		Role:  "model",
		Parts: []*genai.Part{{Text: text}},
	}
	e.LLMResponse = model.LLMResponse{Partial: true}
	e.CustomMetadata = map[string]any{"channel": ChannelStatus}
	return e
}

// EmitToolCall creates a partial event describing a tool invocation.
func EmitToolCall(invocationID, agentName, toolName string, summary string, debug bool, fullArgs, fullResult any) *session.Event {
	data := map[string]any{
		"tool":   toolName,
		"result": summary,
	}
	if debug {
		data["args"] = fullArgs
		data["full_result"] = fullResult
	}

	e := session.NewEvent(invocationID)
	e.Author = agentName
	e.Content = &genai.Content{
		Role:  "model",
		Parts: []*genai.Part{{Text: toolName + ": " + summary}},
	}
	e.LLMResponse = model.LLMResponse{Partial: true}
	e.CustomMetadata = map[string]any{"channel": ChannelToolCall, "data": data}
	return e
}

// EmitToolError creates a partial event describing a tool error.
func EmitToolError(invocationID, agentName, toolName, errMsg string) *session.Event {
	e := session.NewEvent(invocationID)
	e.Author = agentName
	e.Content = &genai.Content{
		Role:  "model",
		Parts: []*genai.Part{{Text: toolName + " error: " + errMsg}},
	}
	e.LLMResponse = model.LLMResponse{Partial: true}
	e.CustomMetadata = map[string]any{"channel": ChannelToolError, "tool": toolName, "error": errMsg}
	return e
}

// EmitContentDelta creates a partial event with incremental text.
func EmitContentDelta(invocationID, agentName, text string) *session.Event {
	e := session.NewEvent(invocationID)
	e.Author = agentName
	e.Content = &genai.Content{
		Role:  "model",
		Parts: []*genai.Part{{Text: text}},
	}
	e.LLMResponse = model.LLMResponse{Partial: true}
	e.CustomMetadata = map[string]any{"channel": ChannelContentDelta}
	return e
}

// EmitFinal creates a non-partial final event with the complete response.
func EmitFinal(invocationID, agentName, text string) *session.Event {
	e := session.NewEvent(invocationID)
	e.Author = agentName
	e.Content = &genai.Content{
		Role:  "model",
		Parts: []*genai.Part{{Text: text}},
	}
	e.LLMResponse = model.LLMResponse{TurnComplete: true}
	e.CustomMetadata = map[string]any{"channel": ChannelFinal}
	return e
}
