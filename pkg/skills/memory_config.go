package skills

import "time"

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

// ReviewConfig controls the rolling-window post-session review
// pipeline for sessions running this skill.
//
// Window / overlap / floor control *when* and *how much* each review
// tick processes for the session. ExcludeEventTypes filters events
// before the LLM sees the transcript (e.g., compaction_summary,
// reasoning, error — event types that rarely carry long-term facts).
type ReviewConfig struct {
	Enabled      bool   `yaml:"enabled"`
	MinToolCalls int    `yaml:"min_tool_calls"`
	Prompt       string `yaml:"prompt"`

	// WindowTokens is the max tokens of transcript the reviewer sends
	// to the LLM per tick. 0 = use agent-level default.
	WindowTokens int `yaml:"window_tokens"`

	// OverlapTokens is how many tokens from the previous window are
	// re-included at the start of the new window to preserve
	// references ("this fact", "that query"). 0 = use default.
	OverlapTokens int `yaml:"overlap_tokens"`

	// FloorAge is the minimum time between consecutive reviews of the
	// same session. Reviews also fire on session close regardless of
	// age. 0 = use default.
	FloorAge time.Duration `yaml:"floor_age"`

	// ExcludeEventTypes skips matching session_events from the review
	// window before tokenization. Merged across active skills with
	// union semantics — if ANY active skill excludes a type, it is
	// dropped.
	ExcludeEventTypes []string `yaml:"exclude_event_types"`
}

// CompactionHints tell the rolling-window compactor what to keep vs.
// discard when summarising old turn groups.
type CompactionHints struct {
	Preserve []string `yaml:"preserve"`
	Discard  []string `yaml:"discard"`
}
