// Package skills loads hugr-agent skills — declarative bundles of
// instructions, tool-provider bindings, and reference documents. The
// file-backed implementation reads from disk on every call (no scan
// cache), so edits on disk are picked up at the next skill_list /
// skill_load.
package skills

// SkillMeta is a compact catalog entry for prompt injection (~50 tokens per skill).
//
// MemoryCategories lists the fully-qualified `<skill>.<cat>` names the
// skill contributes to long-term memory (from its memory.yaml). Stored
// in the catalog so the model can pick the right `category` filter for
// `memory_search` even before the skill is loaded.
type SkillMeta struct {
	Name             string   `json:"name"`
	Description      string   `json:"description"`
	Categories       []string `json:"categories"`
	MemoryCategories []string `json:"memory_categories,omitempty"`
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

// SessionTypeRoot / SessionTypeSubAgent / SessionTypeFork mirror the
// constants in pkg/sessions/store but are duplicated here to avoid an
// import cycle (skills is consumed by sessions, not the other way
// around). Used in AutoloadFor to scope which sessions an autoload
// skill applies to (spec 006).
const (
	SessionTypeRoot     = "root"
	SessionTypeSubAgent = "subagent"
	SessionTypeFork     = "fork"
)

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
	// new session's Create. Combined with AutoloadFor (spec 006) to
	// filter which session types pick the skill up — a skill can be
	// "autoload for root only" (typical for system-management tools),
	// "autoload for both root and subagent" (memory / context tools),
	// or "autoload for subagent only" (future sub-agent constitution).
	Autoload bool
	// AutoloadFor restricts Autoload to specific session types
	// (sessions.session_type values: "root" | "subagent" | "fork").
	// When Autoload is true and AutoloadFor is empty, defaults to
	// ["root"] at parse time so existing skills keep their pre-006
	// behaviour.
	AutoloadFor []string
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
	// SubAgents is the map of specialist roles a skill exposes for
	// sub-agent dispatch (spec 006 phase 1). Each role becomes a
	// `subagent_<skill>_<role>` tool registered on sessions that
	// load this skill. Nil/empty when the skill declares no
	// specialists (the common case for non-domain skills).
	SubAgents map[string]SubAgentSpec
}

// SubAgentSpec defines one specialist role declared in a skill's
// frontmatter under `sub_agents.<role>`. Read at skill-load time
// (mapstructure-decoded from frontmatter); not persistent at runtime.
//
// Fields:
//   - Description (required): human-readable summary the coordinator
//     LLM sees in the dispatch tool's description; should make it
//     obvious when to invoke the role.
//   - Intent: routes the specialist to a model via Router.ModelFor.
//     Empty means the router's default model (typically the strong
//     coordinator model). Set to a cheap-model intent (e.g.
//     "tool_calling") when the role is cheap-compatible.
//   - Instructions (required): system-prompt body for the specialist
//     LLM. Combined at dispatch time with the parent skill body.
//   - Tools: subset of the parent skill's Providers[].Name to expose.
//     Empty means "all of the skill's providers". Names that don't
//     match a Providers[].Name fail validation at skill load.
//   - ToolAllowlist: optional finer-grained tool-name allowlist
//     applied on top of each provider's existing allowlist. Supports
//     exact names and "prefix-*" globs (same syntax as
//     SkillProviderSpec.Tools).
//   - MaxTurns: hard cap on the dispatcher's turn loop. Defaults to
//     15 when omitted; rejected if explicitly set to <= 0.
//   - SummaryMaxTok: cap on the final assistant text returned to the
//     coordinator (rune count, NOT a token estimate). Defaults to
//     800; rejected if explicitly set to <= 0.
type SubAgentSpec struct {
	Description    string   `yaml:"description"    json:"description"     mapstructure:"description"`
	Intent         string   `yaml:"intent"         json:"intent,omitempty" mapstructure:"intent"`
	Instructions   string   `yaml:"instructions"   json:"instructions"    mapstructure:"instructions"`
	Tools          []string `yaml:"tools"          json:"tools,omitempty"          mapstructure:"tools"`
	ToolAllowlist  []string `yaml:"tool_allowlist" json:"tool_allowlist,omitempty" mapstructure:"tool_allowlist"`
	MaxTurns       int      `yaml:"max_turns"      json:"max_turns,omitempty"      mapstructure:"max_turns"`
	SummaryMaxTok  int      `yaml:"summary_max_tokens" json:"summary_max_tokens,omitempty" mapstructure:"summary_max_tokens"`
}

// Default sub-agent spec values used by the frontmatter parser when a
// field is omitted by the skill author.
const (
	defaultSubAgentMaxTurns      = 15
	defaultSubAgentSummaryMaxTok = 800
)

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
