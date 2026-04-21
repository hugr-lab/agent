// Package test provides deterministic adapters for testing.
package models

import (
	"context"
	"encoding/json"
	"iter"

	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

// ScriptedResponse defines one turn of a scripted LLM conversation.
type ScriptedResponse struct {
	// Content is the text response from the model.
	Content string `json:"content,omitempty"`

	// ToolCalls are tool invocations the model makes.
	ToolCalls []ScriptedToolCall `json:"tool_calls,omitempty"`
}

// ScriptedToolCall defines a single tool call in a scripted response.
type ScriptedToolCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

// ScriptedLLM implements model.LLM with a fixed sequence of responses.
// Each call to GenerateContent returns the next response in the script.
type ScriptedLLM struct {
	name      string
	responses []ScriptedResponse
	turn      int
}

// NewScriptedLLM creates a scripted LLM from a sequence of responses.
func NewScriptedLLM(name string, responses []ScriptedResponse) *ScriptedLLM {
	return &ScriptedLLM{name: name, responses: responses}
}

// NewScriptedLLMFromJSON creates a scripted LLM from a JSON fixture.
func NewScriptedLLMFromJSON(name string, data []byte) (*ScriptedLLM, error) {
	var responses []ScriptedResponse
	if err := json.Unmarshal(data, &responses); err != nil {
		return nil, err
	}
	return NewScriptedLLM(name, responses), nil
}

func (m *ScriptedLLM) Name() string { return m.name }

func (m *ScriptedLLM) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if m.turn >= len(m.responses) {
			yield(&model.LLMResponse{
				Content: &genai.Content{
					Role:  "model",
					Parts: []*genai.Part{{Text: "[scripted LLM: no more responses]"}},
				},
				TurnComplete: true,
			}, nil)
			return
		}

		resp := m.responses[m.turn]
		m.turn++

		var parts []*genai.Part

		if resp.Content != "" {
			parts = append(parts, &genai.Part{Text: resp.Content})
		}

		for _, tc := range resp.ToolCalls {
			parts = append(parts, &genai.Part{
				FunctionCall: &genai.FunctionCall{
					Name: tc.Name,
					Args: tc.Args,
				},
			})
		}

		if len(parts) == 0 {
			parts = []*genai.Part{{Text: ""}}
		}

		yield(&model.LLMResponse{
			Content: &genai.Content{
				Role:  "model",
				Parts: parts,
			},
			TurnComplete: true,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     100,
				CandidatesTokenCount: 50,
			},
		}, nil)
	}
}
