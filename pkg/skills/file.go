package skills

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hugr-lab/hugen/pkg/learning"
	"gopkg.in/yaml.v3"
)

// skillFrontmatter is the YAML header inside SKILL.md.
type skillFrontmatter struct {
	Name        string               `yaml:"name"`
	Description string               `yaml:"description"`
	Categories  []string             `yaml:"categories"`
	Autoload    bool                 `yaml:"autoload"`
	Providers   []SkillProviderSpec  `yaml:"providers"`
	References  []frontmatterRefMeta `yaml:"references"`
	NextStep    string               `yaml:"next_step"`
}

// frontmatterRefMeta mirrors SkillRefMeta with YAML tags so
// skill authors can list references with human-written descriptions
// instead of relying on filename-as-description fallback.
type frontmatterRefMeta struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// fileManager reads skills from a directory on every call — no scan cache.
type fileManager struct {
	path string
}

var _ Manager = (*fileManager)(nil)

// NewFileManager returns a file-backed Manager rooted at path.
func NewFileManager(path string) (Manager, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("skills: stat %q: %w", path, err)
	}
	return &fileManager{path: path}, nil
}

// List scans the skills directory and returns compact metadata for every
// valid skill found. Directories without a parseable SKILL.md are skipped.
func (m *fileManager) List(_ context.Context) ([]SkillMeta, error) {
	entries, err := os.ReadDir(m.path)
	if err != nil {
		return nil, fmt.Errorf("skills: read %q: %w", m.path, err)
	}
	var out []SkillMeta
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		fm, ok := readFrontmatter(filepath.Join(m.path, e.Name()))
		if !ok {
			continue
		}
		name := fm.Name
		if name == "" {
			name = e.Name()
		}
		out = append(out, SkillMeta{
			Name:        name,
			Description: fm.Description,
			Categories:  append([]string(nil), fm.Categories...),
		})
	}
	return out, nil
}

// AutoloadNames returns every skill name whose frontmatter has
// `autoload: true`. Order matches List().
func (m *fileManager) AutoloadNames(_ context.Context) ([]string, error) {
	entries, err := os.ReadDir(m.path)
	if err != nil {
		return nil, fmt.Errorf("skills: read %q: %w", m.path, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		fm, ok := readFrontmatter(filepath.Join(m.path, e.Name()))
		if !ok || !fm.Autoload {
			continue
		}
		name := fm.Name
		if name == "" {
			name = e.Name()
		}
		out = append(out, name)
	}
	return out, nil
}

// Load reads a single skill's SKILL.md fresh on every call — the
// on-disk copy is always authoritative.
func (m *fileManager) Load(_ context.Context, name string) (*Skill, error) {
	skillDir := filepath.Join(m.path, name)
	raw, err := os.ReadFile(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		return nil, fmt.Errorf("skills: %q not found: %w", name, err)
	}
	fm, instructions := parseFrontmatter(string(raw))
	canonical := fm.Name
	if canonical == "" {
		canonical = name
	}

	// Expand ${ENV_VAR} in inline endpoints — lets a single skill file
	// stay portable across environments.
	providers := make([]SkillProviderSpec, 0, len(fm.Providers))
	for _, spec := range fm.Providers {
		spec.Endpoint = os.ExpandEnv(spec.Endpoint)
		providers = append(providers, spec)
	}

	var refs []SkillRefMeta
	if len(fm.References) > 0 {
		for _, r := range fm.References {
			refs = append(refs, SkillRefMeta{
				Name:        r.Name,
				Description: r.Description,
			})
		}
	} else {
		refs, _ = listRefs(filepath.Join(skillDir, "references"))
	}

	return &Skill{
		Name:         canonical,
		Description:  fm.Description,
		Categories:   fm.Categories,
		Instructions: instructions,
		Autoload:     fm.Autoload,
		Providers:    providers,
		Refs:         refs,
		NextStep:     fm.NextStep,
		Memory:       loadSkillMemory(skillDir),
	}, nil
}

// loadSkillMemory reads an optional memory.yaml adjacent to SKILL.md.
// Absent file → nil (skill still usable). Parse errors → nil + no
// fatal error; callers fall back to agent-level memory defaults. The
// file is cheap enough to re-read on every Load since the manager
// does not cache skill bodies.
func loadSkillMemory(skillDir string) *learning.SkillMemoryConfig {
	raw, err := os.ReadFile(filepath.Join(skillDir, "memory.yaml"))
	if err != nil {
		return nil
	}
	var cfg learning.SkillMemoryConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil
	}
	return &cfg
}

// Reference returns the raw content of a reference document.
func (m *fileManager) Reference(_ context.Context, skillName, refName string) (string, error) {
	refPath := filepath.Join(m.path, skillName, "references", refName+".md")
	data, err := os.ReadFile(refPath)
	if err != nil {
		return "", fmt.Errorf("skills: ref %q/%q: %w", skillName, refName, err)
	}
	return string(data), nil
}

// RenderCatalog delegates to the package-level helper.
func (m *fileManager) RenderCatalog(skills []SkillMeta) string {
	return RenderCatalog(skills)
}

// ------------------------------------------------------------
// helpers
// ------------------------------------------------------------

func readFrontmatter(skillDir string) (skillFrontmatter, bool) {
	raw, err := os.ReadFile(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		return skillFrontmatter{}, false
	}
	fm, _ := parseFrontmatter(string(raw))
	return fm, true
}

func listRefs(refDir string) ([]SkillRefMeta, error) {
	entries, err := os.ReadDir(refDir)
	if err != nil {
		return nil, err
	}
	var refs []SkillRefMeta
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		refs = append(refs, SkillRefMeta{Name: name, Description: name})
	}
	return refs, nil
}

func parseFrontmatter(raw string) (skillFrontmatter, string) {
	var fm skillFrontmatter
	if !strings.HasPrefix(raw, "---") {
		return fm, raw
	}
	parts := strings.SplitN(raw[3:], "---", 2)
	if len(parts) != 2 {
		return fm, raw
	}
	_ = yaml.Unmarshal([]byte(parts[0]), &fm)
	return fm, strings.TrimSpace(parts[1])
}
