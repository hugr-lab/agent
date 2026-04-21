package skills

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/tools"
	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"
)

// ServiceName is the provider name under which the skills service
// registers in tools.Manager. On-disk skill frontmatter references
// this name via providers: [{provider: _skills}].
const ServiceName = "_skills"

// SessionAccessor is the minimal contract the skill tools need from the
// runtime session manager. Defined locally so pkg/skills does not
// import pkg/sessions (sessions already imports skills → would cycle).
// *sessions.Manager satisfies this structurally.
type SessionAccessor interface {
	Session(id string) (Session, error)
}

// Session is the minimal session view these tools use — consumer-defined
// interface. *sessions.Session satisfies it structurally.
type Session interface {
	ListSkills(ctx context.Context) ([]SkillMeta, error)
	SetCatalog([]SkillMeta) error
	LoadSkill(ctx context.Context, name string) error
	UnloadSkill(ctx context.Context, name string) error
	LoadReference(ctx context.Context, skill, ref string) error
	UnloadReference(ctx context.Context, skill, ref string) error
	SkillMeta(ctx context.Context, name string) DescriptorMeta
	ReadReference(ctx context.Context, skill, ref string) (string, error)
}

// Service is the tools.Provider that exposes skill-lifecycle tools
// (skill_list / skill_load / skill_unload / skill_ref /
// skill_ref_unload). It registers under name "_skills" and delegates
// every Run to the session resolved via SessionAccessor.
type Service struct {
	sm    SessionAccessor
	tools []tool.Tool
}

// NewService constructs the Service. Register it in tools.Manager via
// tools.Manager.AddProvider(svc).
func NewService(sm SessionAccessor) *Service {
	s := &Service{sm: sm}
	s.tools = []tool.Tool{
		&skillListTool{sm: sm},
		&skillLoadTool{sm: sm},
		&skillUnloadTool{sm: sm},
		&skillRefTool{sm: sm},
		&skillRefUnloadTool{sm: sm},
	}
	return s
}

// Name implements tools.Provider.
func (s *Service) Name() string { return ServiceName }

// Tools implements tools.Provider.
func (s *Service) Tools() []tool.Tool { return s.tools }

// sessionFor returns the session that owns this tool invocation, or an
// error if the context has no session id / the session vanished.
func sessionFor(ctx tool.Context, sm SessionAccessor) (Session, error) {
	sid := ctx.SessionID()
	if sid == "" {
		return nil, fmt.Errorf("no session id in tool context")
	}
	return sm.Session(sid)
}

// ------------------------------------------------------------
// skill_list
// ------------------------------------------------------------

type skillListTool struct{ sm SessionAccessor }

func (t *skillListTool) Name() string { return "skill_list" }
func (t *skillListTool) Description() string {
	return "Returns a JSON array of available skills with their names and descriptions. Call this FIRST to discover what skills can be loaded. No parameters required."
}
func (t *skillListTool) IsLongRunning() bool { return false }

func (t *skillListTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: t.Name(), Description: t.Description()}
}

func (t *skillListTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *skillListTool) Run(ctx tool.Context, _ any) (map[string]any, error) {
	sess, err := sessionFor(ctx, t.sm)
	if err != nil {
		return nil, fmt.Errorf("skill_list: %w", err)
	}
	list, err := sess.ListSkills(ctx)
	if err != nil {
		return nil, fmt.Errorf("skill_list: %w", err)
	}
	if err := sess.SetCatalog(list); err != nil {
		return nil, fmt.Errorf("skill_list: SetCatalog: %w", err)
	}

	data, _ := json.Marshal(list)
	return map[string]any{"skills": json.RawMessage(data)}, nil
}

// ------------------------------------------------------------
// skill_load
// ------------------------------------------------------------

type skillLoadTool struct{ sm SessionAccessor }

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

func (t *skillLoadTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
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

	sess, err := sessionFor(ctx, t.sm)
	if err != nil {
		return nil, fmt.Errorf("skill_load: %w", err)
	}
	if err := sess.LoadSkill(ctx, name); err != nil {
		return nil, fmt.Errorf("skill_load: %w", err)
	}
	_ = sess.SetCatalog(nil)

	meta := sess.SkillMeta(ctx, name)
	var refs []map[string]string
	for _, r := range meta.Refs {
		refs = append(refs, map[string]string{
			"name":        r.Name,
			"description": r.Description,
		})
	}
	next := meta.NextStep
	if next == "" {
		if len(refs) == 0 {
			next = "No references available for this skill. Proceed with the skill's data tools."
		} else {
			next = "Pick references that match the user's task and call skill_ref for each before using data tools."
		}
	}
	return map[string]any{
		"loaded":     name,
		"references": refs,
		"next_step":  next,
	}, nil
}

// ------------------------------------------------------------
// skill_unload
// ------------------------------------------------------------

type skillUnloadTool struct{ sm SessionAccessor }

func (t *skillUnloadTool) Name() string { return "skill_unload" }
func (t *skillUnloadTool) Description() string {
	return "Deactivates a previously-loaded skill. Removes its instructions, its MCP tools, and every reference loaded under it from the session context. Use to free context budget when the user's task has moved on to a different domain."
}
func (t *skillUnloadTool) IsLongRunning() bool { return false }

func (t *skillUnloadTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: "OBJECT",
			Properties: map[string]*genai.Schema{
				"name": {
					Type:        "STRING",
					Description: "Skill name to unload, as it appears in the 'Loaded skills' list.",
				},
			},
			Required: []string{"name"},
		},
	}
}

func (t *skillUnloadTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *skillUnloadTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	m, ok := args.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("skill_unload: unexpected args type: %T", args)
	}
	name, _ := m["name"].(string)
	if name == "" {
		return nil, fmt.Errorf("skill_unload: missing required parameter: name")
	}
	sess, err := sessionFor(ctx, t.sm)
	if err != nil {
		return nil, fmt.Errorf("skill_unload: %w", err)
	}
	if err := sess.UnloadSkill(ctx, name); err != nil {
		return nil, fmt.Errorf("skill_unload: %w", err)
	}
	return map[string]any{"unloaded": name}, nil
}

// ------------------------------------------------------------
// skill_ref
// ------------------------------------------------------------

type skillRefTool struct{ sm SessionAccessor }

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
				"skill": {Type: "STRING", Description: "Skill name, e.g. \"hugr-data\""},
				"ref":   {Type: "STRING", Description: "Reference document name as returned by skill_load"},
			},
			Required: []string{"skill", "ref"},
		},
	}
}

func (t *skillRefTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
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
	sess, err := sessionFor(ctx, t.sm)
	if err != nil {
		return nil, fmt.Errorf("skill_ref: %w", err)
	}
	if err := sess.LoadReference(ctx, skill, ref); err != nil {
		return nil, fmt.Errorf("skill_ref: %w", err)
	}

	content, _ := sess.ReadReference(ctx, skill, ref)
	return map[string]any{"loaded": ref, "content": content}, nil
}

// ------------------------------------------------------------
// skill_ref_unload
// ------------------------------------------------------------

type skillRefUnloadTool struct{ sm SessionAccessor }

func (t *skillRefUnloadTool) Name() string { return "skill_ref_unload" }
func (t *skillRefUnloadTool) Description() string {
	return "Removes a previously-loaded reference document from the session prompt. Use to free context budget when a reference is no longer needed for the remaining task."
}
func (t *skillRefUnloadTool) IsLongRunning() bool { return false }

func (t *skillRefUnloadTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: "OBJECT",
			Properties: map[string]*genai.Schema{
				"skill": {Type: "STRING", Description: "Skill the reference belongs to."},
				"ref":   {Type: "STRING", Description: "Reference name shown as [LOADED] in the skill block."},
			},
			Required: []string{"skill", "ref"},
		},
	}
}

func (t *skillRefUnloadTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *skillRefUnloadTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	m, ok := args.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("skill_ref_unload: unexpected args type: %T", args)
	}
	skill, _ := m["skill"].(string)
	ref, _ := m["ref"].(string)
	if skill == "" || ref == "" {
		return nil, fmt.Errorf("skill_ref_unload: missing required parameters: skill, ref")
	}
	sess, err := sessionFor(ctx, t.sm)
	if err != nil {
		return nil, fmt.Errorf("skill_ref_unload: %w", err)
	}
	if err := sess.UnloadReference(ctx, skill, ref); err != nil {
		return nil, fmt.Errorf("skill_ref_unload: %w", err)
	}
	return map[string]any{"unloaded": ref, "skill": skill}, nil
}
