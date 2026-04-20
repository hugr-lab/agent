package learning

import "log/slog"

// Per-skill memory configuration types. Populated by
// pkg/skills/file.go from an optional memory.yaml adjacent to each
// skill's SKILL.md. Nil SkillMemoryConfig means the skill has no
// memory-specific tailoring — the reviewer/compactor fall back to
// the runtime defaults when that skill is active.

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

// MergedConfig is the result of merging SkillMemoryConfig across all
// active skills in a session. Consumed by the reviewer (category
// selection + prompt assembly) and the compactor (preserve/discard
// hints).
type MergedConfig struct {
	Categories      map[string]CategoryConfig
	ReviewEnabled   bool
	MinToolCalls    int
	ReviewPrompt    string
	CompactPreserve []string
	CompactDiscard  []string
}

// Merge combines memory configs from a set of active skill configs
// into a single MergedConfig. Merge rules:
//   - Category names are globally unique: first encountered wins;
//     later collisions are discarded and logged at WARN when a
//     non-nil logger is provided.
//   - Review prompts are concatenated with "## Skill: <name>\n" headers
//     in the order provided by the caller.
//   - Review is enabled if ANY skill enables it; MinToolCalls is the
//     maximum across enabled skills (most-restrictive wins).
//   - Compaction hint lists are unioned with de-duplication preserving
//     first-seen order.
func Merge(configs []NamedConfig) MergedConfig {
	out := MergedConfig{
		Categories: map[string]CategoryConfig{},
	}
	seen := map[string]struct{}{}
	for _, nc := range configs {
		if nc.Config == nil {
			continue
		}
		for name, cat := range nc.Config.Categories {
			if _, dup := out.Categories[name]; dup {
				continue
			}
			out.Categories[name] = cat
		}
		if nc.Config.Review.Enabled {
			out.ReviewEnabled = true
			if nc.Config.Review.MinToolCalls > out.MinToolCalls {
				out.MinToolCalls = nc.Config.Review.MinToolCalls
			}
			if nc.Config.Review.Prompt != "" {
				if out.ReviewPrompt != "" {
					out.ReviewPrompt += "\n\n"
				}
				out.ReviewPrompt += "## Skill: " + nc.Name + "\n" + nc.Config.Review.Prompt
			}
		}
		for _, s := range nc.Config.Compaction.Preserve {
			if _, dup := seen["p:"+s]; dup {
				continue
			}
			seen["p:"+s] = struct{}{}
			out.CompactPreserve = append(out.CompactPreserve, s)
		}
		for _, s := range nc.Config.Compaction.Discard {
			if _, dup := seen["d:"+s]; dup {
				continue
			}
			seen["d:"+s] = struct{}{}
			out.CompactDiscard = append(out.CompactDiscard, s)
		}
	}
	return out
}

// NamedConfig pairs a skill name with its optional memory config.
// Consumers build a slice of these from the active skills of a
// session and pass it to Merge.
type NamedConfig struct {
	Name   string
	Config *SkillMemoryConfig
}

// MergeWithLogger is the logging variant of Merge: it emits a WARN
// entry for every category collision between skills. Useful at
// runtime (reviewer / compactor) so operators can spot conflicting
// skill configs; not appropriate for pure-logic unit tests.
func MergeWithLogger(configs []NamedConfig, logger *slog.Logger) MergedConfig {
	out := MergedConfig{Categories: map[string]CategoryConfig{}}
	seen := map[string]struct{}{}
	origin := map[string]string{} // category → winning skill
	for _, nc := range configs {
		if nc.Config == nil {
			continue
		}
		for name, cat := range nc.Config.Categories {
			if first, dup := origin[name]; dup {
				if logger != nil {
					logger.Warn("learning.Merge: category collision",
						"category", name, "winner", first, "loser", nc.Name)
				}
				continue
			}
			out.Categories[name] = cat
			origin[name] = nc.Name
		}
		if nc.Config.Review.Enabled {
			out.ReviewEnabled = true
			if nc.Config.Review.MinToolCalls > out.MinToolCalls {
				out.MinToolCalls = nc.Config.Review.MinToolCalls
			}
			if nc.Config.Review.Prompt != "" {
				if out.ReviewPrompt != "" {
					out.ReviewPrompt += "\n\n"
				}
				out.ReviewPrompt += "## Skill: " + nc.Name + "\n" + nc.Config.Review.Prompt
			}
		}
		for _, s := range nc.Config.Compaction.Preserve {
			if _, dup := seen["p:"+s]; dup {
				continue
			}
			seen["p:"+s] = struct{}{}
			out.CompactPreserve = append(out.CompactPreserve, s)
		}
		for _, s := range nc.Config.Compaction.Discard {
			if _, dup := seen["d:"+s]; dup {
				continue
			}
			seen["d:"+s] = struct{}{}
			out.CompactDiscard = append(out.CompactDiscard, s)
		}
	}
	return out
}
