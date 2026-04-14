// Package hugrmodel implements ADK model.LLM interface using Hugr GraphQL.
package hugr

import (
	"encoding/json"
	"fmt"

	"github.com/hugr-lab/query-engine/types"

	"google.golang.org/genai"
)

// adkToHugrMessages converts ADK Content messages to JSON-encoded strings
// for the Hugr GraphQL chat_completion messages parameter.
func adkToHugrMessages(contents []*genai.Content) ([]string, error) {
	var messages []string
	for _, c := range contents {
		msgs, err := contentToHugrMessages(c)
		if err != nil {
			return nil, fmt.Errorf("convert content (role=%s): %w", c.Role, err)
		}
		messages = append(messages, msgs...)
	}
	return messages, nil
}

func contentToHugrMessages(c *genai.Content) ([]string, error) {
	role := mapRole(c.Role)
	var result []string

	for _, p := range c.Parts {
		switch {
		case p.Text != "":
			msg := types.LLMMessage{
				Role:    role,
				Content: p.Text,
			}
			b, err := json.Marshal(msg)
			if err != nil {
				return nil, fmt.Errorf("marshal text message: %w", err)
			}
			result = append(result, string(b))

		case p.FunctionCall != nil:
			msg := types.LLMMessage{
				Role:    "assistant",
				Content: "",
				ToolCalls: []types.LLMToolCall{{
					ID:        p.FunctionCall.ID,
					Name:      p.FunctionCall.Name,
					Arguments: p.FunctionCall.Args,
				}},
			}
			b, err := json.Marshal(msg)
			if err != nil {
				return nil, fmt.Errorf("marshal function call message: %w", err)
			}
			result = append(result, string(b))

		case p.FunctionResponse != nil:
			msg := types.LLMMessage{
				Role:       "tool",
				Content:    formatFunctionResponse(p.FunctionResponse.Response),
				ToolCallID: p.FunctionResponse.ID,
			}
			b, err := json.Marshal(msg)
			if err != nil {
				return nil, fmt.Errorf("marshal function response message: %w", err)
			}
			result = append(result, string(b))
		}
	}

	// If content has multiple text parts, merge them into one message.
	if len(result) == 0 && len(c.Parts) > 0 {
		msg := types.LLMMessage{Role: role, Content: ""}
		b, _ := json.Marshal(msg)
		result = append(result, string(b))
	}

	return result, nil
}

func mapRole(role string) string {
	switch role {
	case "model":
		return "assistant"
	case "function":
		return "tool"
	default:
		return role
	}
}

func formatFunctionResponse(resp map[string]any) string {
	if resp == nil {
		return "{}"
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return fmt.Sprintf("%v", resp)
	}
	return string(b)
}

// adkToHugrTools converts ADK genai.Tool FunctionDeclarations to JSON-encoded
// strings for the Hugr GraphQL chat_completion tools parameter.
// ADK stores tool declarations in req.Config.Tools (not req.Tools).
//
// ADK mcptoolset puts MCP InputSchema into ParametersJsonSchema (raw JSON),
// not Parameters (*genai.Schema). We prefer ParametersJsonSchema when available
// because it preserves the original JSON Schema format that Hugr expects.
func adkToHugrTools(genaiTools []*genai.Tool) ([]string, error) {
	if len(genaiTools) == 0 {
		return nil, nil
	}

	var result []string
	for _, t := range genaiTools {
		if t == nil || len(t.FunctionDeclarations) == 0 {
			continue
		}
		for _, decl := range t.FunctionDeclarations {
			// Prefer ParametersJsonSchema (raw JSON Schema from MCP)
			// over Parameters (*genai.Schema which uses UPPERCASE types).
			var params any
			if decl.ParametersJsonSchema != nil {
				params = decl.ParametersJsonSchema
			} else if decl.Parameters != nil {
				params = decl.Parameters
			}

			hugrTool := types.LLMTool{
				Name:        decl.Name,
				Description: decl.Description,
				Parameters:  params,
			}
			b, err := json.Marshal(hugrTool)
			if err != nil {
				return nil, fmt.Errorf("marshal tool %q: %w", decl.Name, err)
			}
			result = append(result, string(b))
		}
	}
	return result, nil
}

// hugrResultToADKContent converts a types.LLMResult to ADK Content.
func hugrResultToADKContent(result types.LLMResult) *genai.Content {
	var parts []*genai.Part

	if result.Content != "" {
		parts = append(parts, &genai.Part{Text: result.Content})
	}

	for _, tc := range result.ToolCalls {
		args := normalizeArgs(tc.Arguments)
		parts = append(parts, &genai.Part{
			FunctionCall: &genai.FunctionCall{
				ID:   tc.ID,
				Name: tc.Name,
				Args: args,
			},
		})
	}

	if len(parts) == 0 {
		parts = append(parts, &genai.Part{Text: ""})
	}

	return &genai.Content{
		Role:  "model",
		Parts: parts,
	}
}

func normalizeArgs(v any) map[string]any {
	switch args := v.(type) {
	case map[string]any:
		return args
	case string:
		var m map[string]any
		if err := json.Unmarshal([]byte(args), &m); err != nil {
			return map[string]any{"raw": args}
		}
		return m
	default:
		if v == nil {
			return nil
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			return map[string]any{"raw": string(b)}
		}
		return m
	}
}

// mapFinishReason converts Hugr finish_reason to ADK FinishReason.
func mapFinishReason(reason string) genai.FinishReason {
	switch reason {
	case "stop":
		return genai.FinishReasonStop
	case "length", "max_tokens":
		return genai.FinishReasonMaxTokens
	case "tool_use":
		return genai.FinishReasonStop
	default:
		return genai.FinishReasonOther
	}
}
