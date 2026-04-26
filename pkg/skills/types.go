// Package skills loads hugr-agent skills — declarative bundles of
// instructions, tool-provider bindings, and reference documents. The
// file-backed implementation reads from disk on every call (no scan
// cache), so edits on disk are picked up at the next skill_list /
// skill_load.
package skills

import "fmt"

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
//   - AsyncHint (spec 007): the role's preferred dispatch mode when the
//     coordinator has a choice. One of "sync" | "async" | "auto"
//     (default "auto" — coordinator decides per request). Parser rejects
//     any other value.
//   - CanSpawn (spec 007): authorises the role to spawn further missions
//     via the `spawn_sub_mission` tool. Default false. The tool registers
//     on the role's session only when this is true.
//   - MaxDepth (spec 007): cap on spawning depth measured from the root
//     coordinator. 0 means "use the agent-wide cap"; positive values
//     override downward only. Must be >= 0.
type SubAgentSpec struct {
	Description   string   `yaml:"description"    json:"description"     mapstructure:"description"`
	Intent        string   `yaml:"intent"         json:"intent,omitempty" mapstructure:"intent"`
	Instructions  string   `yaml:"instructions"   json:"instructions"    mapstructure:"instructions"`
	Tools         []string `yaml:"tools"          json:"tools,omitempty"          mapstructure:"tools"`
	ToolAllowlist []string `yaml:"tool_allowlist" json:"tool_allowlist,omitempty" mapstructure:"tool_allowlist"`
	MaxTurns      int      `yaml:"max_turns"      json:"max_turns,omitempty"      mapstructure:"max_turns"`
	SummaryMaxTok int      `yaml:"summary_max_tokens" json:"summary_max_tokens,omitempty" mapstructure:"summary_max_tokens"`

	// Phase-2 additions (spec 007). Additive; phase-1 skills load unchanged.
	AsyncHint string `yaml:"async_hint" json:"async_hint,omitempty" mapstructure:"async_hint"`
	CanSpawn  bool   `yaml:"can_spawn"  json:"can_spawn,omitempty"  mapstructure:"can_spawn"`
	MaxDepth  int    `yaml:"max_depth"  json:"max_depth,omitempty"  mapstructure:"max_depth"`

	// Phase-4 additions (spec 009). Optional declarative gate rules
	// consulted by *approvals.Gate before each sub-agent tool call.
	// Sits in the resolution chain between the persistent
	// tool_policies overrides and the hardcoded default. nil ⇒ chain
	// falls through to the default.
	ApprovalRules *SubAgentApprovalRules `yaml:"approval_rules" json:"approval_rules,omitempty" mapstructure:"approval_rules"`

	// RequiredSkills (spec 009 / US5) is the union of additional
	// skills loaded into the child session at dispatch time on top
	// of the parent skill. Allows roles to compose tools across
	// multiple skills (e.g. cross_domain_analyst needs both
	// hugr-data and python-sandbox). nil ⇒ only the parent skill
	// is loaded. Missing skill names fail dispatch fast.
	RequiredSkills []string `yaml:"required_skills" json:"required_skills,omitempty" mapstructure:"required_skills"`

	// ParallelValidation (spec 009 / US6) enables the executor to
	// spawn TWO sibling missions on dispatch and ask the
	// coordinator to reconcile their answers. nil ⇒ single-spawn
	// behaviour preserved.
	ParallelValidation *ParallelValidationSpec `yaml:"parallel_validation" json:"parallel_validation,omitempty" mapstructure:"parallel_validation"`
}

// ParallelValidationSpec configures a role for the parallel-
// validation pattern (spec 009 phase 4 / US6). When `enabled` is
// true, every dispatch of this role spawns two sibling sub-agent
// missions with the same parent_session_id and distinguishable
// mission strings. Both siblings run concurrently; when both reach
// terminal status, the coordinator's next turn sees both
// agent_result events and reconciles them via the merge_strategy.
//
// Phase 4 ships only `agent_choice` — the coordinator picks a
// winner based on agent_result metadata. `user_choice` and `merge`
// are documented (parsed by the YAML decoder) but the executor
// rejects them at dispatch time with a clear error.
type ParallelValidationSpec struct {
	// Enabled toggles the parallel spawn behaviour. false / nil
	// spec ⇒ single-spawn (default phase-1/2/3 behaviour).
	Enabled bool `yaml:"enabled" json:"enabled" mapstructure:"enabled"`

	// When is a free-form rationale describing under what
	// circumstances the role should run with parallel validation.
	// Currently informational (used only in role descriptions /
	// audit logs). Phase-4.5 may expand this to a runtime gate.
	When string `yaml:"when" json:"when,omitempty" mapstructure:"when"`

	// MergeStrategy is how the coordinator reconciles the two
	// sibling outputs. Phase 4 supports `agent_choice` only.
	// Default empty string is treated as `agent_choice`.
	MergeStrategy string `yaml:"merge_strategy" json:"merge_strategy,omitempty" mapstructure:"merge_strategy"`
}

// Recognised merge strategies (only the first is implemented in
// phase 4; the others parse but error at dispatch time).
const (
	MergeStrategyAgentChoice = "agent_choice"
	MergeStrategyUserChoice  = "user_choice"
	MergeStrategyMerge       = "merge"
)

// Validate reports whether the spec is dispatchable. Returns nil
// when Enabled is false (parallel validation off). Otherwise
// requires merge_strategy to be agent_choice — other strategies
// are recognised but fail dispatch in phase 4.
func (s *ParallelValidationSpec) Validate() error {
	if s == nil || !s.Enabled {
		return nil
	}
	strategy := s.MergeStrategy
	if strategy == "" {
		strategy = MergeStrategyAgentChoice
	}
	switch strategy {
	case MergeStrategyAgentChoice:
		return nil
	case MergeStrategyUserChoice, MergeStrategyMerge:
		return fmt.Errorf("parallel_validation merge_strategy %q is recognised but not supported in phase 4 — use %q", strategy, MergeStrategyAgentChoice)
	default:
		return fmt.Errorf("parallel_validation merge_strategy %q is unknown — use %q", strategy, MergeStrategyAgentChoice)
	}
}

// SubAgentApprovalRules is the declarative HITL gate ruleset
// declared in a role's frontmatter. Mirrors
// approvals.FrontmatterApprovalRules; lives here so pkg/skills does
// not have to import pkg/approvals (one-way dependency direction:
// pkg/agent + pkg/runtime + pkg/approvals consume; pkg/skills declares).
//
// Each list accepts exact tool names (`data-execute_mutation`) or
// prefix globs ending in `*` (`data-*`).
type SubAgentApprovalRules struct {
	// AutoApprove tools bypass the gate entirely (sets policy to
	// always_allowed via the frontmatter step of the resolution chain).
	AutoApprove []string `yaml:"auto_approve" json:"auto_approve,omitempty" mapstructure:"auto_approve"`

	// RequireUser tools always pause the mission and surface an
	// approval envelope.
	RequireUser []string `yaml:"require_user" json:"require_user,omitempty" mapstructure:"require_user"`

	// ParentCanApprove (declared but currently treated as RequireUser
	// in phase 4 — parent grant inheritance lands in a follow-up).
	ParentCanApprove []string `yaml:"parent_can_approve" json:"parent_can_approve,omitempty" mapstructure:"parent_can_approve"`

	// Risk overrides per tool name. Default risk is `medium`; entries
	// here let the role mark specific tools as `high` or `low` for
	// envelope rendering.
	Risk map[string]string `yaml:"risk" json:"risk,omitempty" mapstructure:"risk"`
}

// Valid values for SubAgentSpec.AsyncHint. The parser treats empty
// string as DefaultSubAgentAsyncHint.
const (
	SubAgentAsyncHintSync  = "sync"
	SubAgentAsyncHintAsync = "async"
	SubAgentAsyncHintAuto  = "auto"
)

// Default sub-agent spec values used by the frontmatter parser when a
// field is omitted by the skill author.
const (
	defaultSubAgentMaxTurns      = 15
	defaultSubAgentSummaryMaxTok = 800
	defaultSubAgentAsyncHint     = SubAgentAsyncHintAuto
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
