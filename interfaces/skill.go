// Package interfaces defines environment-agnostic contracts for the hugr-agent runtime.
package interfaces

import "context"

// SkillMeta is a compact catalog entry for prompt injection (~50 tokens per skill).
type SkillMeta struct {
	Name        string   `json:"name" yaml:"name"`
	Description string   `json:"description" yaml:"description"`
	Categories  []string `json:"categories" yaml:"categories"`
}

// SkillFull is a loaded skill with core instructions and reference manifest.
type SkillFull struct {
	Name         string
	Instructions string         // always-loaded core text
	References   []SkillRefMeta // available deep-dive documents
	MCPEndpoint  string         // optional MCP server URL (may contain ${ENV_VAR} placeholders)
}

// SkillRefMeta describes an available reference document within a skill.
type SkillRefMeta struct {
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description" yaml:"description"`
}

// SkillProvider abstracts skill storage and loading.
type SkillProvider interface {
	// ListMeta returns compact metadata for all available skills.
	ListMeta(ctx context.Context) ([]SkillMeta, error)

	// LoadFull loads core instructions + reference manifest for a skill.
	LoadFull(ctx context.Context, name string) (*SkillFull, error)

	// LoadRef loads a specific reference document from a skill.
	LoadRef(ctx context.Context, skill, ref string) (string, error)
}
