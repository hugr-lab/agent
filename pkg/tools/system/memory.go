package system

import (
	"encoding/json"
	"fmt"

	"github.com/hugr-lab/hugen/interfaces"
	"github.com/hugr-lab/hugen/pkg/tools"
	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"
)

// NewMemorySuite returns the LLM-facing tools exposed through the
// `_memory` system provider: memory_search, memory_linked, memory_stats.
// Session-scratchpad tools (memory_note / memory_clear_note) and
// write paths land in Phase 4 (US2).
//
// Each tool resolves its session from tool.Context and delegates to
// HubDB. The suite itself is stateless.
func NewMemorySuite(sm interfaces.SessionManager, hub interfaces.HubDB) []tool.Tool {
	if hub == nil {
		return nil
	}
	return []tool.Tool{
		&memorySearchTool{sm: sm, hub: hub},
		&memoryLinkedTool{sm: sm, hub: hub},
		&memoryStatsTool{sm: sm, hub: hub},
	}
}

// ------------------------------------------------------------
// memory_search
// ------------------------------------------------------------

type memorySearchTool struct {
	sm  interfaces.SessionManager
	hub interfaces.HubDB
}

func (t *memorySearchTool) Name() string { return "memory_search" }
func (t *memorySearchTool) Description() string {
	return "Searches long-term memory for facts matching a natural-language query. Filter by tags (AND) or category. Returns at most 5 facts by default, each with age_days and expires_in_days so you can judge freshness."
}
func (t *memorySearchTool) IsLongRunning() bool { return false }

func (t *memorySearchTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: "OBJECT",
			Properties: map[string]*genai.Schema{
				"query": {
					Type:        "STRING",
					Description: "Natural-language query. Matched against fact content with keyword fallback when no embedding is available.",
				},
				"tags": {
					Type:        "ARRAY",
					Items:       &genai.Schema{Type: "STRING"},
					Description: "Optional tag filter (AND). Facts must carry every listed tag.",
				},
				"category": {
					Type:        "STRING",
					Description: "Optional category filter, e.g. \"schema\", \"query_template\", \"anti_pattern\".",
				},
				"limit": {
					Type:        "INTEGER",
					Description: "Max results to return. Default 5, hard cap 20.",
				},
			},
			Required: []string{"query"},
		},
	}
}

func (t *memorySearchTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *memorySearchTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	m, ok := args.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("memory_search: unexpected args: %T", args)
	}
	query, _ := m["query"].(string)
	category, _ := m["category"].(string)
	var tags []string
	if raw, ok := m["tags"].([]any); ok {
		for _, v := range raw {
			if s, _ := v.(string); s != "" {
				tags = append(tags, s)
			}
		}
	}
	limit := 5
	if v, ok := m["limit"].(float64); ok && v > 0 {
		limit = int(v)
		if limit > 20 {
			limit = 20
		}
	}
	results, err := t.hub.Search(ctx, query, nil, interfaces.SearchOpts{
		Category: category,
		Tags:     tags,
		Limit:    limit,
	})
	if err != nil {
		return nil, fmt.Errorf("memory_search: %w", err)
	}
	data, _ := json.Marshal(results)
	return map[string]any{"results": json.RawMessage(data), "count": len(results)}, nil
}

// ------------------------------------------------------------
// memory_linked
// ------------------------------------------------------------

type memoryLinkedTool struct {
	sm  interfaces.SessionManager
	hub interfaces.HubDB
}

func (t *memoryLinkedTool) Name() string { return "memory_linked" }
func (t *memoryLinkedTool) Description() string {
	return "Returns facts reachable from a given memory item through outgoing links, up to the requested depth. Use to navigate from a schema fact to related query templates and anti-patterns."
}
func (t *memoryLinkedTool) IsLongRunning() bool { return false }

func (t *memoryLinkedTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: "OBJECT",
			Properties: map[string]*genai.Schema{
				"id":    {Type: "STRING", Description: "Memory item ID (e.g. mem_ag01_...)"},
				"depth": {Type: "INTEGER", Description: "Traversal depth. Default 1, max 3."},
			},
			Required: []string{"id"},
		},
	}
}

func (t *memoryLinkedTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *memoryLinkedTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	m, ok := args.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("memory_linked: unexpected args: %T", args)
	}
	id, _ := m["id"].(string)
	if id == "" {
		return nil, fmt.Errorf("memory_linked: missing required parameter: id")
	}
	depth := 1
	if v, ok := m["depth"].(float64); ok && v > 0 {
		depth = int(v)
		if depth > 3 {
			depth = 3
		}
	}
	results, err := t.hub.GetLinked(ctx, id, depth)
	if err != nil {
		return nil, fmt.Errorf("memory_linked: %w", err)
	}
	data, _ := json.Marshal(results)
	return map[string]any{"results": json.RawMessage(data), "count": len(results)}, nil
}

// ------------------------------------------------------------
// memory_stats
// ------------------------------------------------------------

type memoryStatsTool struct {
	sm  interfaces.SessionManager
	hub interfaces.HubDB
}

func (t *memoryStatsTool) Name() string { return "memory_stats" }
func (t *memoryStatsTool) Description() string {
	return "Returns a compact summary of long-term memory: total count, active count, counts by category, oldest/newest fact dates."
}
func (t *memoryStatsTool) IsLongRunning() bool { return false }

func (t *memoryStatsTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: t.Name(), Description: t.Description()}
}

func (t *memoryStatsTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *memoryStatsTool) Run(ctx tool.Context, _ any) (map[string]any, error) {
	stats, err := t.hub.Stats(ctx)
	if err != nil {
		return nil, fmt.Errorf("memory_stats: %w", err)
	}
	data, _ := json.Marshal(stats)
	return map[string]any{"stats": json.RawMessage(data)}, nil
}
