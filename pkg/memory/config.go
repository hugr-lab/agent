package memory

import (
	"log/slog"

	"github.com/hugr-lab/hugen/pkg/skills"
)

// MergedConfig is the result of merging skills.SkillMemoryConfig across
// all active skills in a session. Consumed by the reviewer (category
// selection + prompt assembly) and the compactor (preserve/discard
// hints).
type MergedConfig struct {
	Categories      map[string]skills.CategoryConfig
	ReviewEnabled   bool
	MinToolCalls    int
	ReviewPrompt    string
	CompactPreserve []string
	CompactDiscard  []string
}

// NamedConfig pairs a skill name with its optional memory config.
// Consumers build a slice of these from the active skills of a
// session and pass it to Merge.
type NamedConfig struct {
	Name   string
	Config *skills.SkillMemoryConfig
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
	out := MergedConfig{Categories: map[string]skills.CategoryConfig{}}
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

// MergeWithLogger is the logging variant of Merge: it emits a WARN
// entry for every category collision between skills.
func MergeWithLogger(configs []NamedConfig, logger *slog.Logger) MergedConfig {
	out := MergedConfig{Categories: map[string]skills.CategoryConfig{}}
	seen := map[string]struct{}{}
	origin := map[string]string{}
	for _, nc := range configs {
		if nc.Config == nil {
			continue
		}
		for name, cat := range nc.Config.Categories {
			if first, dup := origin[name]; dup {
				if logger != nil {
					logger.Warn("memory.Merge: category collision",
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
