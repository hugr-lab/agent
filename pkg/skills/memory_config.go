package skills

// Per-skill memory configuration types. Populated by skills.fileManager
// from an optional memory.yaml adjacent to each skill's SKILL.md. The
// runtime memory-review and compaction pipelines read these via
// Skill.Memory when a skill is active in a session.

// SkillMemoryConfig is the full per-skill memory configuration.
type SkillMemoryConfig struct {
	Categories map[string]CategoryConfig `yaml:"categories"`
	Review     ReviewConfig              `yaml:"review"`
	Compaction CompactionHints           `yaml:"compaction"`
}

// CategoryConfig declares how the reviewer should treat facts of a
// particular category — initial score, volatility bucket, and a short
// hint for the LLM describing what tags to attach.
type CategoryConfig struct {
	Description  string  `yaml:"description"`
	Volatility   string  `yaml:"volatility"` // stable|slow|moderate|fast|volatile
	InitialScore float64 `yaml:"initial_score"`
	TagsHint     string  `yaml:"tags_hint"`
}

// ReviewConfig controls the post-session review pipeline for sessions
// running this skill.
type ReviewConfig struct {
	Enabled      bool   `yaml:"enabled"`
	MinToolCalls int    `yaml:"min_tool_calls"`
	Prompt       string `yaml:"prompt"`
}

// CompactionHints tell the rolling-window compactor what to keep vs.
// discard when summarising old turn groups.
type CompactionHints struct {
	Preserve []string `yaml:"preserve"`
	Discard  []string `yaml:"discard"`
}
