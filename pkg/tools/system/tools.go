// Package system provides the agent's built-in system tools:
// skill-list, skill-load, skill-ref.
package system

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/hugr-lab/hugen/interfaces"
	hugen "github.com/hugr-lab/hugen/pkg/agent"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/mcptoolset"
	"google.golang.org/genai"
)

// Deps holds shared dependencies for all system tools.
type Deps struct {
	Skills      interfaces.SkillProvider
	Prompt      *hugen.PromptBuilder
	Toolset     *hugen.DynamicToolset
	Tokens      *hugen.TokenEstimator
	Transport   http.RoundTripper // for MCP connections with auth
	Logger      *slog.Logger
	activeSkill string // currently loaded skill name (empty = none)
}

// packTool registers a tool's function declaration into the LLM request.
// Reimplements toolutils.PackTool since toolinternal is not importable.
func packTool(req *model.LLMRequest, name string, decl *genai.FunctionDeclaration, t any) error {
	if req.Tools == nil {
		req.Tools = make(map[string]any)
	}
	if _, ok := req.Tools[name]; ok {
		return fmt.Errorf("duplicate tool: %q", name)
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

	// Also inject catalog into prompt so LLM sees skill descriptions.
	t.deps.Prompt.SetCatalog(skills)

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

	// Unload previous skill if any.
	if prev := t.deps.activeSkill; prev != "" && prev != name {
		t.deps.Toolset.RemoveToolset("skill:" + prev)
		t.deps.Prompt.ClearSkill()
		t.deps.Logger.Info("skill_load: unloaded previous skill", "skill", prev)
	}
	t.deps.activeSkill = name

	// Inject skill instructions into prompt and clear catalog.
	t.deps.Prompt.SetSkillInstructions(skill.Instructions)
	t.deps.Prompt.ClearCatalog()

	// Connect MCP toolset if the skill has an endpoint.
	if skill.MCPEndpoint != "" {
		transport := &sdkmcp.StreamableClientTransport{
			Endpoint:             skill.MCPEndpoint,
			DisableStandaloneSSE: true,
			HTTPClient:           &http.Client{Transport: t.deps.Transport},
		}
		mcpTools, err := mcptoolset.New(mcptoolset.Config{
			Transport: transport,
		})
		if err != nil {
			t.deps.Logger.Error("skill_load: MCP connect failed", "skill", name, "err", err)
			return map[string]any{
				"loaded":    name,
				"mcp_error": err.Error(),
			}, nil
		}
		t.deps.Toolset.AddToolset("skill:"+name, mcpTools)
		t.deps.Logger.Info("skill_load: MCP tools connected", "skill", name, "endpoint", skill.MCPEndpoint)
	}

	// Build reference list for the response.
	refs := make([]string, 0, len(skill.References))
	for _, r := range skill.References {
		refs = append(refs, r.Name)
	}

	t.deps.Logger.Info("skill_load: loaded", "skill", name, "refs", len(refs))

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

	content, err := t.deps.Skills.LoadRef(ctx, skill, ref)
	if err != nil {
		return nil, fmt.Errorf("skill_ref: %w", err)
	}

	// Append reference to the prompt.
	t.deps.Prompt.AppendReference(ref, content)

	return map[string]any{
		"loaded":  ref,
		"content": content,
	}, nil
}
