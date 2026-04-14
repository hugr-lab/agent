// Package system provides the agent's built-in system tools:
// skill-list, skill-load, skill-ref.
package system

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/hugr-lab/hugen/interfaces"
	hugen "github.com/hugr-lab/hugen/pkg/agent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"
)

// Deps holds shared dependencies for all system tools.
type Deps struct {
	Skills      interfaces.SkillProvider
	Prompt      *hugen.PromptBuilder
	Toolset     *hugen.DynamicToolset
	Tokens      *hugen.TokenEstimator
	Logger      *slog.Logger
	MCPToolsets map[string]tool.Toolset // endpoint URL → pre-created MCP toolset
}

// packTool registers a tool's function declaration into the LLM request.
// Reimplements toolutils.PackTool since toolinternal is not importable.
func packTool(req *model.LLMRequest, name string, decl *genai.FunctionDeclaration, t any) error {
	if req.Tools == nil {
		req.Tools = make(map[string]any)
	}
	if _, ok := req.Tools[name]; ok {
		return nil // already registered in this request
	}
	req.Tools[name] = t

	if decl == nil {
		return nil
	}
	if req.Config == nil {
		req.Config = &genai.GenerateContentConfig{}
	}
	var funcTool *genai.Tool
	for _, t := range req.Config.Tools {
		if t != nil && t.FunctionDeclarations != nil {
			funcTool = t
			break
		}
	}
	if funcTool == nil {
		req.Config.Tools = append(req.Config.Tools, &genai.Tool{
			FunctionDeclarations: []*genai.FunctionDeclaration{decl},
		})
	} else {
		funcTool.FunctionDeclarations = append(funcTool.FunctionDeclarations, decl)
	}
	return nil
}

// --- skill-list ---

type skillListTool struct {
	deps *Deps
}

func (t *skillListTool) Name() string { return "skill_list" }
func (t *skillListTool) Description() string {
	return "Returns a JSON array of available skills with their names and descriptions. Call this FIRST to discover what skills can be loaded. No parameters required."
}
func (t *skillListTool) IsLongRunning() bool { return false }

func (t *skillListTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
	}
}

func (t *skillListTool) ProcessRequest(ctx tool.Context, req *model.LLMRequest) error {
	return packTool(req, t.Name(), t.Declaration(), t)
}

func (t *skillListTool) Run(ctx tool.Context, _ any) (map[string]any, error) {
	skills, err := t.deps.Skills.ListMeta(ctx)
	if err != nil {
		return nil, fmt.Errorf("skill_list: %w", err)
	}

	// Inject catalog into this session's prompt so LLM sees skill descriptions.
	t.deps.Prompt.SetCatalog(ctx.SessionID(), skills)

	data, _ := json.Marshal(skills)
	return map[string]any{"skills": json.RawMessage(data)}, nil
}

// --- skill-load ---

type skillLoadTool struct {
	deps *Deps
}

func (t *skillLoadTool) Name() string { return "skill_load" }
func (t *skillLoadTool) Description() string {
	return "Activates a skill by name, loading its instructions and data tools. Call skill_list first to get available names. After loading, new domain-specific tools become available."
}
func (t *skillLoadTool) IsLongRunning() bool { return false }

func (t *skillLoadTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: "OBJECT",
			Properties: map[string]*genai.Schema{
				"name": {
					Type:        "STRING",
					Description: "Skill name exactly as returned by skill_list, e.g. \"hugr-data\"",
				},
			},
			Required: []string{"name"},
		},
	}
}

func (t *skillLoadTool) ProcessRequest(ctx tool.Context, req *model.LLMRequest) error {
	return packTool(req, t.Name(), t.Declaration(), t)
}

func (t *skillLoadTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	m, ok := args.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("skill_load: unexpected args type: %T", args)
	}
	name, _ := m["name"].(string)
	if name == "" {
		return nil, fmt.Errorf("skill_load: missing required parameter: name")
	}

	skill, err := t.deps.Skills.LoadFull(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("skill_load: %w", err)
	}

	sessionID := ctx.SessionID()

	// Unload previous skill instructions/references and MCP tools if switching skills.
	if prev := t.deps.Prompt.ActiveSkill(sessionID); prev != "" && prev != name {
		t.deps.Prompt.ClearSkill(sessionID)
		t.deps.Toolset.RemoveSessionToolset(sessionID, "mcp:"+prev)
		t.deps.Logger.Info("skill_load: unloaded previous skill", "skill", prev, "session", sessionID)
	}

	// Inject skill instructions into this session's prompt and clear catalog.
	t.deps.Prompt.SetSkillInstructions(sessionID, name, skill.Instructions)
	t.deps.Prompt.ClearCatalog(sessionID)

	// Add MCP tools for this skill to the session (pre-created at startup).
	if skill.MCPEndpoint != "" {
		if mcpTS, ok := t.deps.MCPToolsets[skill.MCPEndpoint]; ok {
			t.deps.Toolset.AddSessionToolset(sessionID, "mcp:"+name, mcpTS)
			t.deps.Logger.Info("skill_load: MCP tools added to session", "skill", name, "session", sessionID)
		} else {
			t.deps.Logger.Warn("skill_load: MCP endpoint not pre-configured", "endpoint", skill.MCPEndpoint, "skill", name)
		}
	}

	// Build reference list for the response.
	refs := make([]string, 0, len(skill.References))
	for _, r := range skill.References {
		refs = append(refs, r.Name)
	}

	t.deps.Logger.Info("skill_load: loaded", "skill", name, "refs", len(refs), "session", sessionID)

	return map[string]any{
		"loaded":     name,
		"references": refs,
	}, nil
}

// --- skill-ref ---

type skillRefTool struct {
	deps *Deps
}

func (t *skillRefTool) Name() string { return "skill_ref" }
func (t *skillRefTool) Description() string {
	return "Loads a reference document from a skill for detailed knowledge (e.g. query syntax, filter operators). The list of available references is returned by skill_load."
}
func (t *skillRefTool) IsLongRunning() bool { return false }

func (t *skillRefTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: "OBJECT",
			Properties: map[string]*genai.Schema{
				"skill": {
					Type:        "STRING",
					Description: "Skill name, e.g. \"hugr-data\"",
				},
				"ref": {
					Type:        "STRING",
					Description: "Reference document name as returned by skill_load, e.g. \"filters\"",
				},
			},
			Required: []string{"skill", "ref"},
		},
	}
}

func (t *skillRefTool) ProcessRequest(ctx tool.Context, req *model.LLMRequest) error {
	return packTool(req, t.Name(), t.Declaration(), t)
}

func (t *skillRefTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	m, ok := args.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("skill_ref: unexpected args type: %T", args)
	}
	skill, _ := m["skill"].(string)
	ref, _ := m["ref"].(string)
	if skill == "" || ref == "" {
		return nil, fmt.Errorf("skill_ref: missing required parameters: skill, ref")
	}

	// Validate that the requested skill is the one loaded in this session.
	if active := t.deps.Prompt.ActiveSkill(ctx.SessionID()); active != skill {
		return nil, fmt.Errorf("skill_ref: skill %q is not loaded (active: %q) — call skill_load first", skill, active)
	}

	content, err := t.deps.Skills.LoadRef(ctx, skill, ref)
	if err != nil {
		return nil, fmt.Errorf("skill_ref: %w", err)
	}

	// Append reference to this session's prompt.
	t.deps.Prompt.AppendReference(ctx.SessionID(), ref, content)

	return map[string]any{
		"loaded":  ref,
		"content": content,
	}, nil
}
