// Package skills loads hugr-agent skills — declarative bundles of
// instructions, tool-provider bindings, and reference documents. The
// file-backed implementation reads from disk on every call (no scan
// cache), so edits on disk are picked up at the next skill_list /
// skill_load.
package skills

// SkillMeta is a compact catalog entry for prompt injection (~50 tokens per skill).
type SkillMeta struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Categories  []string `json:"categories"`
}

// SkillRefMeta describes an available reference document within a skill.
type SkillRefMeta struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// DescriptorMeta bundles the reference list and workflow hint that
// skill_load returns to the LLM. Populated from SKILL.md frontmatter
// via Session.SkillMeta.
type DescriptorMeta struct {
	Refs     []SkillRefMeta
	NextStep string
}

// Skill is a fully-loaded skill ready for the prompt + tool wiring.
type Skill struct {
	Name         string
	// Version is a free-form string (frontmatter `version:`). When
	// LoadSkill sees a different value than the previously loaded
	// one on the same session, the old per-skill bindings are torn
	// down and replaced. Empty means "0" for comparison purposes —
	// treat-as-changed on first load.
	Version      string
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
	Memory *SkillMemoryConfig
}

// SkillProviderSpec is one tool-source binding declared by a skill.
// `Name` is the required identifier the skill uses to reference
// this provider in tools.Manager. Two specs in different skills
// with the same Name+Provider share the underlying global raw
// provider; with the same Name+Endpoint they conflict (first
// load wins).
//
// Exactly one of Provider or Endpoint must be set:
//   - Provider: reference a provider configured in config.yaml by
//     name. The Auth/AuthType/AuthHeader* fields on the spec are
//     ignored — auth is taken from the named provider's config.
//   - Endpoint: inline MCP endpoint. A raw provider is synthesised
//     and registered in tools.Manager under
//     "skill-inline/<skill>/<Name>". AuthType selects the transport
//     wrapping:
//       - "hugr"   → Bearer token from cfg.Auth[Auth]
//       - "header" → custom header (AuthHeaderName/AuthHeaderValue)
//       - "auto"   → no wrap (MCP server handles auth itself, future)
//       - ""       → hugr when Auth set, auto otherwise (back-compat)
//
// Tools is an optional allowlist. Empty = all tools from the raw
// provider are exposed. Supports exact names and `prefix-*` globs.
type SkillProviderSpec struct {
	// Name is the required per-skill handle. Filtered binding is
	// registered under "skill/<skillName>/<Name>".
	Name string `yaml:"name" json:"name"`

	Provider string `yaml:"provider" json:"provider"`
	Endpoint string `yaml:"endpoint" json:"endpoint"`

	// Auth is the name of a cfg.Auth entry (only meaningful when
	// AuthType == "hugr" or empty and a Bearer token wrap is
	// needed for inline endpoints).
	Auth string `yaml:"auth" json:"auth"`

	// AuthType selects the transport wrapping for inline endpoints.
	// Ignored when Provider is set (config.yaml wins).
	AuthType        string `yaml:"auth_type" json:"auth_type"`
	AuthHeaderName  string `yaml:"auth_header_name" json:"auth_header_name"`
	AuthHeaderValue string `yaml:"auth_header_value" json:"auth_header_value"`

	Tools []string `yaml:"tools" json:"tools"`
}
