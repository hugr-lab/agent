// Package skills loads hugr-agent skills — declarative bundles of
// instructions, tool-provider bindings, and reference documents. The
// file-backed implementation reads from disk on every call (no scan
// cache), so edits on disk are picked up at the next skill_list /
// skill_load.
package skills

import "github.com/hugr-lab/hugen/pkg/learning"

// SkillMeta is a compact catalog entry for prompt injection (~50 tokens per skill).
type SkillMeta struct {
	Name        string   `json:"name" yaml:"name"`
	Description string   `json:"description" yaml:"description"`
	Categories  []string `json:"categories" yaml:"categories"`
}

// SkillRefMeta describes an available reference document within a skill.
type SkillRefMeta struct {
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description" yaml:"description"`
}

// Skill is a fully-loaded skill ready for the prompt + tool wiring.
type Skill struct {
	Name         string
	Description  string
	Categories   []string
	Instructions string
	// Autoload tells the SessionManager to load this skill on every
	// new session's Create.
	Autoload bool
	// Providers is the set of tool-provider bindings this skill
	// contributes to the session. Each entry is either a reference
	// to a configured provider or an inline MCP endpoint. The
	// optional Tools allowlist filters what subset is exposed.
	Providers []SkillProviderSpec
	Refs      []SkillRefMeta
	// NextStep is an optional workflow hint returned to the LLM from
	// skill_load. When empty, skill_load falls back to a generic
	// "read refs before data tools" phrase.
	NextStep string
	// Memory is the per-skill memory configuration loaded from an
	// optional memory.yaml file adjacent to SKILL.md. Nil when the
	// file is absent or malformed — the reviewer/compactor then
	// fall back to agent-level defaults.
	Memory *learning.SkillMemoryConfig
}

// SkillProviderSpec is one tool-source binding declared by a skill.
// Exactly one of Provider or Endpoint must be set:
//   - Provider: reference a provider configured in config.yaml by name.
//     Auth is taken from that provider's config; Auth field here is
//     ignored.
//   - Endpoint: inline MCP endpoint — creates an anonymous raw provider
//     scoped to this skill. Auth is optional (name of auth config).
//
// Tools is an optional allowlist. Empty = all tools from the raw
// provider are exposed. Supports exact names and `prefix-*` globs.
type SkillProviderSpec struct {
	Provider string   `yaml:"provider" json:"provider"`
	Endpoint string   `yaml:"endpoint" json:"endpoint"`
	Auth     string   `yaml:"auth" json:"auth"`
	Tools    []string `yaml:"tools" json:"tools"`
}
