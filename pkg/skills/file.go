package skills

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// skillFrontmatter is the YAML header inside SKILL.md.
type skillFrontmatter struct {
	Name        string                  `yaml:"name"`
	Version     string                  `yaml:"version"`
	Description string                  `yaml:"description"`
	Categories  []string                `yaml:"categories"`
	Autoload    bool                    `yaml:"autoload"`
	AutoloadFor []string                `yaml:"autoload_for"`
	Providers   []SkillProviderSpec     `yaml:"providers"`
	References  []frontmatterRefMeta    `yaml:"references"`
	NextStep    string                  `yaml:"next_step"`
	SubAgents   map[string]SubAgentSpec `yaml:"sub_agents"`
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
		skillDir := filepath.Join(m.path, e.Name())
		out = append(out, SkillMeta{
			Name:             name,
			Description:      fm.Description,
			Categories:       append([]string(nil), fm.Categories...),
			MemoryCategories: memoryCategoryNames(name, skillDir),
		})
	}
	return out, nil
}

// memoryCategoryNames returns the fully-qualified `<skill>.<cat>`
// names from the skill's memory.yaml, sorted for stable output.
// Returns nil when the file is absent or malformed — List degrades to
// the no-category catalog entry in those cases.
func memoryCategoryNames(skillName, skillDir string) []string {
	cfg := loadSkillMemory(skillDir)
	if cfg == nil || len(cfg.Categories) == 0 {
		return nil
	}
	out := make([]string, 0, len(cfg.Categories))
	for cat := range cfg.Categories {
		out = append(out, skillName+"."+cat)
	}
	sort.Strings(out)
	return out
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

// AutoloadNamesFor returns autoload skills whose AutoloadFor list
// includes sessionType. Skills that omit autoload_for are treated as
// `["root"]` (mirrors normalizeAutoloadFor's parse-time default), so
// callers asking for "root" sessions see the pre-006 behaviour and
// callers asking for "subagent" sessions see only opt-in skills.
//
// An empty sessionType is treated as "root" — defensive default for
// callers that haven't migrated to passing the discriminator yet.
func (m *fileManager) AutoloadNamesFor(_ context.Context, sessionType string) ([]string, error) {
	if sessionType == "" {
		sessionType = SessionTypeRoot
	}
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
		applicable := normalizeAutoloadFor(fm.Autoload, fm.AutoloadFor)
		if !contains(applicable, sessionType) {
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

func contains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
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

	// Expand ${ENV_VAR} in inline endpoints + header auth values —
	// lets a single skill file stay portable across environments.
	// Also validates shape: Name is always required; exactly one of
	// Provider or Endpoint must be set.
	providers := make([]SkillProviderSpec, 0, len(fm.Providers))
	for i, spec := range fm.Providers {
		if spec.Name == "" {
			return nil, fmt.Errorf("skills: %q provider[%d]: name is required", canonical, i)
		}
		if spec.Provider == "" && spec.Endpoint == "" {
			return nil, fmt.Errorf("skills: %q provider %q: either provider or endpoint is required", canonical, spec.Name)
		}
		if spec.Provider != "" && spec.Endpoint != "" {
			return nil, fmt.Errorf("skills: %q provider %q: provider and endpoint are mutually exclusive", canonical, spec.Name)
		}
		spec.Endpoint = os.ExpandEnv(spec.Endpoint)
		spec.AuthHeaderName = os.ExpandEnv(spec.AuthHeaderName)
		spec.AuthHeaderValue = os.ExpandEnv(spec.AuthHeaderValue)
		providers = append(providers, spec)
	}

	var refs []SkillRefMeta
	if len(fm.References) > 0 {
		for _, r := range fm.References {
			refs = append(refs, SkillRefMeta(r))
		}
	} else {
		refs, _ = listRefs(filepath.Join(skillDir, "references"))
	}

	subAgents, err := normalizeSubAgents(canonical, providers, fm.SubAgents)
	if err != nil {
		return nil, err
	}

	autoloadFor := normalizeAutoloadFor(fm.Autoload, fm.AutoloadFor)

	return &Skill{
		Name:         canonical,
		Version:      fm.Version,
		Description:  fm.Description,
		Categories:   fm.Categories,
		Instructions: instructions,
		Autoload:     fm.Autoload,
		AutoloadFor:  autoloadFor,
		Providers:    providers,
		Refs:         refs,
		NextStep:     fm.NextStep,
		Memory:       loadSkillMemory(skillDir),
		SubAgents:    subAgents,
	}, nil
}

// normalizeAutoloadFor implements the spec-006 default: when a skill
// declares `autoload: true` but omits `autoload_for`, we pick the
// pre-006 behaviour (root sessions only). Skills with `autoload:
// false` keep AutoloadFor at nil — applyAutoload only consults the
// list when Autoload is true.
func normalizeAutoloadFor(autoload bool, declared []string) []string {
	if !autoload {
		return nil
	}
	if len(declared) == 0 {
		return []string{SessionTypeRoot}
	}
	out := make([]string, 0, len(declared))
	seen := make(map[string]bool, len(declared))
	for _, v := range declared {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(out) == 0 {
		// Edge case: declared was non-empty but every entry was blank
		// (e.g. `autoload_for: [""]`). Fall back to the safe default.
		return []string{SessionTypeRoot}
	}
	return out
}

// normalizeSubAgents validates and applies defaults to the
// `sub_agents:` frontmatter block. Errors identify skill + role + the
// specific failure so misconfigured skills surface at load time
// rather than at dispatch time. Returns nil when the skill declares
// no specialists.
func normalizeSubAgents(skillName string, providers []SkillProviderSpec, raw map[string]SubAgentSpec) (map[string]SubAgentSpec, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	providerNames := make(map[string]bool, len(providers))
	for _, p := range providers {
		providerNames[p.Name] = true
	}

	out := make(map[string]SubAgentSpec, len(raw))
	for role, spec := range raw {
		if role == "" {
			return nil, fmt.Errorf("skills: %q: sub_agents has empty role name", skillName)
		}
		if strings.TrimSpace(spec.Description) == "" {
			return nil, fmt.Errorf("skills: %q sub_agent %q: description is required", skillName, role)
		}
		if strings.TrimSpace(spec.Instructions) == "" {
			return nil, fmt.Errorf("skills: %q sub_agent %q: instructions are required", skillName, role)
		}
		for _, tool := range spec.Tools {
			if !providerNames[tool] {
				return nil, fmt.Errorf("skills: %q sub_agent %q: tool %q is not in skill providers", skillName, role, tool)
			}
		}
		// Apply defaults; explicit non-positive values are an error
		// (catches typos like `max_turns: 0`).
		switch {
		case spec.MaxTurns == 0:
			spec.MaxTurns = defaultSubAgentMaxTurns
		case spec.MaxTurns < 0:
			return nil, fmt.Errorf("skills: %q sub_agent %q: max_turns must be > 0 (got %d)", skillName, role, spec.MaxTurns)
		}
		switch {
		case spec.SummaryMaxTok == 0:
			spec.SummaryMaxTok = defaultSubAgentSummaryMaxTok
		case spec.SummaryMaxTok < 0:
			return nil, fmt.Errorf("skills: %q sub_agent %q: summary_max_tokens must be > 0 (got %d)", skillName, role, spec.SummaryMaxTok)
		}
		out[role] = spec
	}
	return out, nil
}

// loadSkillMemory reads an optional memory.yaml adjacent to SKILL.md.
// Absent file → nil (skill still usable). Parse errors → nil + no
// fatal error; callers fall back to agent-level memory defaults. The
// file is cheap enough to re-read on every Load since the manager
// does not cache skill bodies.
func loadSkillMemory(skillDir string) *SkillMemoryConfig {
	raw, err := os.ReadFile(filepath.Join(skillDir, "memory.yaml"))
	if err != nil {
		return nil
	}
	var cfg SkillMemoryConfig
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
