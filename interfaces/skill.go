// Package interfaces defines environment-agnostic contracts for the hugr-agent runtime.
package interfaces

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
