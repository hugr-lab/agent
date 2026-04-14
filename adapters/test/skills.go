package test

import (
	"context"
	"fmt"

	"github.com/hugr-lab/hugen/interfaces"
)

// StaticSkillProvider returns pre-configured skills for testing.
type StaticSkillProvider struct {
	skills map[string]*interfaces.SkillFull
	refs   map[string]string // "skill/ref" → content
}

var _ interfaces.SkillProvider = (*StaticSkillProvider)(nil)

// NewStaticSkillProvider creates a test skill provider from a map of skills.
func NewStaticSkillProvider(skills map[string]*interfaces.SkillFull) *StaticSkillProvider {
	if skills == nil {
		skills = make(map[string]*interfaces.SkillFull)
	}
	return &StaticSkillProvider{
		skills: skills,
		refs:   make(map[string]string),
	}
}

// AddRef adds a reference document for testing.
func (p *StaticSkillProvider) AddRef(skill, ref, content string) {
	p.refs[skill+"/"+ref] = content
}

func (p *StaticSkillProvider) ListMeta(_ context.Context) ([]interfaces.SkillMeta, error) {
	var result []interfaces.SkillMeta
	for _, s := range p.skills {
		result = append(result, interfaces.SkillMeta{
			Name:        s.Name,
			Description: s.Instructions[:min(len(s.Instructions), 80)],
		})
	}
	return result, nil
}

func (p *StaticSkillProvider) LoadFull(_ context.Context, name string) (*interfaces.SkillFull, error) {
	s, ok := p.skills[name]
	if !ok {
		return nil, fmt.Errorf("test/skills: skill %q not found", name)
	}
	return s, nil
}

func (p *StaticSkillProvider) LoadRef(_ context.Context, skill, ref string) (string, error) {
	if content, ok := p.refs[skill+"/"+ref]; ok {
		return content, nil
	}
	return "", fmt.Errorf("test/skills: ref %q/%q not found", skill, ref)
}
