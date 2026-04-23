package sessions

import (
	"encoding/json"
	"time"

	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
	"google.golang.org/adk/model"
	adksession "google.golang.org/adk/session"
	"google.golang.org/genai"
)

// replaySkillState walks session events in order and returns the set
// of skills left active after the last matching (loaded, unloaded)
// pair for each name.
func replaySkillState(events []sessstore.Event) map[string]struct{} {
	active := map[string]struct{}{}
	for _, ev := range events {
		switch ev.EventType {
		case sessstore.EventTypeSkillLoaded:
			name := skillNameFromEvent(ev)
			if name != "" {
				active[name] = struct{}{}
			}
		case sessstore.EventTypeSkillUnloaded:
			name := skillNameFromEvent(ev)
			if name != "" {
				delete(active, name)
			}
		}
	}
	return active
}

// skillNameFromEvent prefers ev.Content (the canonical location) but
// falls back to Metadata.skill — early rows used the metadata path.
func skillNameFromEvent(ev sessstore.Event) string {
	if ev.Content != "" {
		return ev.Content
	}
	if ev.Metadata != nil {
		if v, ok := ev.Metadata["skill"].(string); ok {
			return v
		}
	}
	return ""
}

// convertToADKEvent maps a hub.db.session_events row back into an
// adksession.Event. Returns ok=false for event types that don't
// belong in the LLM-facing conversation stream (skill_*, note,
// error, forked).
func convertToADKEvent(ev sessstore.Event) (*adksession.Event, bool) {
	switch ev.EventType {
	case sessstore.EventTypeUserMessage:
		return &adksession.Event{
			ID:        ev.ID,
			Author:    orDefault(ev.Author, "user"),
			Timestamp: timestamp(ev),
			LLMResponse: model.LLMResponse{
				Content: &genai.Content{
					Role:  "user",
					Parts: []*genai.Part{{Text: ev.Content}},
				},
				TurnComplete: true,
			},
		}, true

	case sessstore.EventTypeLLMResponse:
		var meta sessstore.LLMResponseMeta
		_ = decodeMetadata(ev.Metadata, &meta)
		resp := model.LLMResponse{
			Content: &genai.Content{
				Role:  "model",
				Parts: []*genai.Part{{Text: ev.Content}},
			},
			ModelVersion: meta.Model,
			TurnComplete: true,
		}
		if meta.PromptTokens != 0 || meta.CompletionTokens != 0 {
			resp.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     int32(meta.PromptTokens),
				CandidatesTokenCount: int32(meta.CompletionTokens),
				TotalTokenCount:      int32(meta.PromptTokens + meta.CompletionTokens),
			}
		}
		return &adksession.Event{
			ID:          ev.ID,
			Author:      orDefault(ev.Author, "model"),
			Timestamp:   timestamp(ev),
			LLMResponse: resp,
		}, true

	case sessstore.EventTypeToolCall:
		return &adksession.Event{
			ID:        ev.ID,
			Author:    orDefault(ev.Author, "model"),
			Timestamp: timestamp(ev),
			LLMResponse: model.LLMResponse{
				Content: &genai.Content{
					Role: "model",
					Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
						Name: ev.ToolName,
						Args: cloneArgs(ev.ToolArgs),
					}}},
				},
				TurnComplete: true,
			},
		}, true

	case sessstore.EventTypeToolResult:
		response := map[string]any{}
		if ev.ToolResult != "" {
			// tool_result is a JSON-encoded blob when the tool emitted
			// structured data; fall back to wrapping raw text for
			// LLMs that ignore the shape.
			var decoded any
			if err := json.Unmarshal([]byte(ev.ToolResult), &decoded); err == nil {
				if m, ok := decoded.(map[string]any); ok {
					response = m
				} else {
					response["result"] = decoded
				}
			} else {
				response["result"] = ev.ToolResult
			}
		}
		return &adksession.Event{
			ID:        ev.ID,
			Author:    orDefault(ev.Author, "tool"),
			Timestamp: timestamp(ev),
			LLMResponse: model.LLMResponse{
				Content: &genai.Content{
					Role: "user",
					Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
						Name:     ev.ToolName,
						Response: response,
					}}},
				},
				TurnComplete: true,
			},
		}, true
	}
	return nil, false
}

func decodeMetadata(src map[string]any, dst any) error {
	if len(src) == 0 {
		return nil
	}
	raw, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, dst)
}

func cloneArgs(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func timestamp(ev sessstore.Event) time.Time {
	if ev.CreatedAt.IsZero() {
		return time.Now().UTC()
	}
	return ev.CreatedAt
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
