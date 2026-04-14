// Package file provides file-based adapter implementations for standalone mode.
package file

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hugr-lab/agent/interfaces"
	"gopkg.in/yaml.v3"
)

// skillFrontmatter is the YAML frontmatter in SKILL.md files.
type skillFrontmatter struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Categories  []string `yaml:"categories"`
}

// mcpConfig is the content of mcp.yaml.
type mcpConfig struct {
	Endpoint string `yaml:"endpoint"`
}

// SkillProvider loads skills from a local directory.
//
// Expected layout:
//
//	{path}/{name}/SKILL.md           YAML frontmatter + markdown instructions
//	{path}/{name}/mcp.yaml           optional MCP endpoint config
//	{path}/{name}/references/*.md    reference documents
type SkillProvider struct {
	path string
}

var _ interfaces.SkillProvider = (*SkillProvider)(nil)

// NewSkillProvider creates a file-based skill provider rooted at path.
func NewSkillProvider(path string) *SkillProvider {
	return &SkillProvider{path: path}
}

func (p *SkillProvider) ListMeta(_ context.Context) ([]interfaces.SkillMeta, error) {
	entries, err := os.ReadDir(p.path)
	if err != nil {
		return nil, fmt.Errorf("file/skills: list: %w", err)
	}

	var result []interfaces.SkillMeta
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		fm, err := p.readFrontmatter(e.Name())
		if err != nil {
			continue // skip dirs without valid SKILL.md
		}
		result = append(result, interfaces.SkillMeta{
			Name:        fm.Name,
			Description: fm.Description,
			Categories:  fm.Categories,
		})
	}
	return result, nil
}

func (p *SkillProvider) LoadFull(_ context.Context, name string) (*interfaces.SkillFull, error) {
	skillDir := filepath.Join(p.path, name)
	if _, err := os.Stat(skillDir); err != nil {
		return nil, fmt.Errorf("file/skills: skill %q not found: %w", name, err)
	}

	raw, err := os.ReadFile(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		return nil, fmt.Errorf("file/skills: read SKILL.md: %w", err)
	}

	fm, instructions := parseFrontmatter(string(raw))

	// Load reference manifest.
	refs, _ := p.listRefs(name)

	// Load MCP endpoint.
	var endpoint string
	mcpPath := filepath.Join(skillDir, "mcp.yaml")
	if data, err := os.ReadFile(mcpPath); err == nil {
		var mc mcpConfig
		if yaml.Unmarshal(data, &mc) == nil {
			endpoint = os.ExpandEnv(mc.Endpoint)
		}
	}

	return &interfaces.SkillFull{
		Name:         fm.Name,
		Instructions: instructions,
		References:   refs,
		MCPEndpoint:  endpoint,
	}, nil
}

func (p *SkillProvider) LoadRef(_ context.Context, skill, ref string) (string, error) {
	refPath := filepath.Join(p.path, skill, "references", ref+".md")
	data, err := os.ReadFile(refPath)
	if err != nil {
		return "", fmt.Errorf("file/skills: ref %q/%q not found: %w", skill, ref, err)
	}
	return string(data), nil
}

func (p *SkillProvider) readFrontmatter(name string) (*skillFrontmatter, error) {
	raw, err := os.ReadFile(filepath.Join(p.path, name, "SKILL.md"))
	if err != nil {
		return nil, err
	}
	fm, _ := parseFrontmatter(string(raw))
	if fm.Name == "" {
		fm.Name = name
	}
	return &fm, nil
}

func (p *SkillProvider) listRefs(name string) ([]interfaces.SkillRefMeta, error) {
	refDir := filepath.Join(p.path, name, "references")
	entries, err := os.ReadDir(refDir)
	if err != nil {
		return nil, err
	}

	var refs []interfaces.SkillRefMeta
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		refName := strings.TrimSuffix(e.Name(), ".md")
		refs = append(refs, interfaces.SkillRefMeta{
			Name:        refName,
			Description: refName, // simple default; SKILL.md references section can override
		})
	}
	return refs, nil
}

// parseFrontmatter splits YAML frontmatter from markdown content.
func parseFrontmatter(raw string) (skillFrontmatter, string) {
	var fm skillFrontmatter
	if !strings.HasPrefix(raw, "---") {
		return fm, raw
	}
	parts := strings.SplitN(raw[3:], "---", 2)
	if len(parts) != 2 {
		return fm, raw
	}
	yaml.Unmarshal([]byte(parts[0]), &fm)
	return fm, strings.TrimSpace(parts[1])
}
