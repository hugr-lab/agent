package memory

import (
	"log/slog"
	"time"

	"github.com/hugr-lab/hugen/pkg/skills"
)

// Config holds memory-domain YAML settings read from `memory:` in
// config.yaml. VolatilityDuration drives the reviewer / verifier;
// Consolidation drives the consolidator; Scheduler tunes the
// memory-workers pass inside pkg/scheduler — the scheduler itself
// has no YAML-owned config because its tuning parameters are really
// memory-specific (review delay, consolidation cron).
type Config struct {
	VolatilityDuration map[string]time.Duration `mapstructure:"volatility_duration"`
	Consolidation      ConsolidationConfig      `mapstructure:"consolidation"`
	Scheduler          SchedulerConfig          `mapstructure:"scheduler"`

	// Review tunes the rolling-window session reviewer defaults used
	// when a session has no active skills declaring review knobs.
	Review ReviewDefaults `mapstructure:"review"`
}

// ReviewDefaults provides agent-level fallbacks for rolling-window
// review parameters. Per-skill memory.yaml can override any of these
// via skills.ReviewConfig.
type ReviewDefaults struct {
	WindowTokens      int           `mapstructure:"window_tokens"`
	OverlapTokens     int           `mapstructure:"overlap_tokens"`
	FloorAge          time.Duration `mapstructure:"floor_age"`
	ExcludeEventTypes []string      `mapstructure:"exclude_event_types"`
}

// ConsolidationConfig tunes the daily hypothesis-consolidation pass.
type ConsolidationConfig struct {
	HypothesisExpiry time.Duration `mapstructure:"hypothesis_expiry"`
}

// SchedulerConfig — tuning for the memory-workers pass driven by
// pkg/scheduler. Owned by memory (these are memory-worker cadence
// knobs, not scheduler-mechanics knobs).
type SchedulerConfig struct {
	Interval        time.Duration `mapstructure:"interval"`
	ReviewDelay     time.Duration `mapstructure:"review_delay"`
	ConsolidationAt string        `mapstructure:"consolidation_at"`
}

// MergedConfig is the result of merging skills.SkillMemoryConfig across
// all active skills in a session. Consumed by the reviewer (category
// selection + prompt assembly + rolling-window tuning) and the
// compactor (preserve/discard hints).
//
// Categories are prefixed with `<skill>.<cat>` — skill namespaces
// keep per-domain categories isolated. CategoryOrigin maps the
// prefixed key back to the declaring skill name for logging.
type MergedConfig struct {
	Categories     map[string]skills.CategoryConfig
	CategoryOrigin map[string]string

	ReviewEnabled bool
	MinToolCalls  int
	ReviewPrompt  string

	// Rolling-window aggregates — 0 when no active skill defined a
	// value (caller falls back to agent-level ReviewDefaults).
	WindowTokens  int           // MIN across active skills
	OverlapTokens int           // MAX across active skills
	FloorAge      time.Duration // MIN across active skills

	// ExcludeEventTypes — union of per-skill exclude lists.
	ExcludeEventTypes map[string]struct{}

	// IncludeEventTypes — union of per-skill include lists. Wins over
	// the agent-level DefaultExcludeEventTypes default (a skill can
	// pull a default-excluded type back into its review window).
	// Per-skill ExcludeEventTypes still wins over IncludeEventTypes
	// from the SAME skill — but cross-skill, include from any skill
	// overrides default-exclude.
	IncludeEventTypes map[string]struct{}

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
// into a single MergedConfig. See file header for rules.
func Merge(configs []NamedConfig) MergedConfig {
	return mergeConfigs(configs, nil)
}

// MergeWithLogger is Merge with a logger available for diagnostics
// (currently used only when the caller wants to surface skill load
// warnings; with prefixed categories, genuine collisions across
// skills are impossible).
func MergeWithLogger(configs []NamedConfig, logger *slog.Logger) MergedConfig {
	return mergeConfigs(configs, logger)
}

func mergeConfigs(configs []NamedConfig, logger *slog.Logger) MergedConfig {
	out := MergedConfig{
		Categories:        map[string]skills.CategoryConfig{},
		CategoryOrigin:    map[string]string{},
		ExcludeEventTypes: map[string]struct{}{},
		IncludeEventTypes: map[string]struct{}{},
	}
	seenCompact := map[string]struct{}{}

	for _, nc := range configs {
		if nc.Config == nil {
			continue
		}

		// Categories: prefix with skill name so per-domain categories
		// don't collide (e.g., hugr-data.schema vs _memory.user_preferences).
		for name, cat := range nc.Config.Categories {
			full := nc.Name + "." + name
			if _, dup := out.Categories[full]; dup {
				// Same skill re-merged; keep first (shouldn't happen in
				// practice — each skill appears once).
				continue
			}
			out.Categories[full] = cat
			out.CategoryOrigin[full] = nc.Name
		}

		rc := nc.Config.Review
		if rc.Enabled {
			out.ReviewEnabled = true
			if rc.MinToolCalls > out.MinToolCalls {
				out.MinToolCalls = rc.MinToolCalls
			}
			if rc.Prompt != "" {
				if out.ReviewPrompt != "" {
					out.ReviewPrompt += "\n\n"
				}
				out.ReviewPrompt += "## Skill: " + nc.Name + "\n" + rc.Prompt
			}
		}

		// Rolling-window aggregates — take values regardless of
		// review.enabled so a disabled-but-configured skill can still
		// hint at tuning (minor; mainly simplifies merge semantics).
		if rc.WindowTokens > 0 {
			if out.WindowTokens == 0 || rc.WindowTokens < out.WindowTokens {
				out.WindowTokens = rc.WindowTokens
			}
		}
		if rc.OverlapTokens > 0 && rc.OverlapTokens > out.OverlapTokens {
			out.OverlapTokens = rc.OverlapTokens
		}
		if rc.FloorAge > 0 {
			if out.FloorAge == 0 || rc.FloorAge < out.FloorAge {
				out.FloorAge = rc.FloorAge
			}
		}
		for _, et := range rc.ExcludeEventTypes {
			if et == "" {
				continue
			}
			out.ExcludeEventTypes[et] = struct{}{}
		}
		for _, et := range rc.IncludeEventTypes {
			if et == "" {
				continue
			}
			out.IncludeEventTypes[et] = struct{}{}
		}

		for _, s := range nc.Config.Compaction.Preserve {
			if _, dup := seenCompact["p:"+s]; dup {
				continue
			}
			seenCompact["p:"+s] = struct{}{}
			out.CompactPreserve = append(out.CompactPreserve, s)
		}
		for _, s := range nc.Config.Compaction.Discard {
			if _, dup := seenCompact["d:"+s]; dup {
				continue
			}
			seenCompact["d:"+s] = struct{}{}
			out.CompactDiscard = append(out.CompactDiscard, s)
		}
	}
	_ = logger // retained for future diagnostics; no-op now
	return out
}
