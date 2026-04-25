package models

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"

	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

// ScriptedPlannerMission is a single planner-output node used by
// ScriptedPlannerResponse to build a JSON envelope matching the
// pkg/missions planner contract (see contracts/planner.md §2).
type ScriptedPlannerMission struct {
	ID    int    `json:"id"`
	Skill string `json:"skill"`
	Role  string `json:"role"`
	Task  string `json:"task"`
}

// ScriptedPlannerEdge is a single planner-output dependency edge.
type ScriptedPlannerEdge struct {
	From int `json:"from"`
	To   int `json:"to"`
}

// ScriptedPlannerResponse renders a strict-JSON planner response as a
// single string; tests wire it as the Content of a ScriptedResponse so
// the planner's parser exercises the real contract on every run.
func ScriptedPlannerResponse(missions []ScriptedPlannerMission, edges []ScriptedPlannerEdge) string {
	payload := struct {
		Missions []ScriptedPlannerMission `json:"missions"`
		Edges    []ScriptedPlannerEdge    `json:"edges"`
	}{Missions: missions, Edges: edges}
	if payload.Edges == nil {
		payload.Edges = []ScriptedPlannerEdge{}
	}
	b, err := json.Marshal(payload)
	if err != nil {
		// impossible for the shapes above — panic keeps the helper pure
		panic(fmt.Errorf("ScriptedPlannerResponse: marshal: %w", err))
	}
	return string(b)
}

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

// Turns returns the number of GenerateContent calls served so far.
// Useful in tests that want to assert "specialist used N turns".
func (m *ScriptedLLM) Turns() int { return m.turn }

// Reset rewinds the script to the first response. Useful when reusing
// a ScriptedLLM across sub-tests.
func (m *ScriptedLLM) Reset() { m.turn = 0 }

// NewScriptedLLM creates a scripted LLM from a sequence of responses.
func NewScriptedLLM(name string, responses []ScriptedResponse) *ScriptedLLM {
	return &ScriptedLLM{name: name, responses: responses}
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
