package memory

import (
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/skills"
	"github.com/stretchr/testify/assert"
)

func TestMerge_CategoriesPrefixedBySkill(t *testing.T) {
	a := NamedConfig{Name: "hugr-data", Config: &skills.SkillMemoryConfig{
		Categories: map[string]skills.CategoryConfig{
			"schema": {Volatility: "stable", InitialScore: 0.8},
		},
	}}
	b := NamedConfig{Name: "web", Config: &skills.SkillMemoryConfig{
		Categories: map[string]skills.CategoryConfig{
			"schema": {Volatility: "volatile", InitialScore: 0.2},
			"extra":  {Volatility: "slow", InitialScore: 0.6},
		},
	}}
	got := Merge([]NamedConfig{a, b})
	// Categories are namespaced by skill name; no collision possible.
	assert.Equal(t, "stable", got.Categories["hugr-data.schema"].Volatility)
	assert.Equal(t, "volatile", got.Categories["web.schema"].Volatility)
	assert.Equal(t, 0.6, got.Categories["web.extra"].InitialScore)
	// CategoryOrigin links back to declaring skill.
	assert.Equal(t, "hugr-data", got.CategoryOrigin["hugr-data.schema"])
	assert.Equal(t, "web", got.CategoryOrigin["web.schema"])
}

func TestMerge_ReviewPromptsConcatenated(t *testing.T) {
	a := NamedConfig{Name: "hugr-data", Config: &skills.SkillMemoryConfig{
		Review: skills.ReviewConfig{Enabled: true, Prompt: "Extract schema facts."},
	}}
	b := NamedConfig{Name: "web", Config: &skills.SkillMemoryConfig{
		Review: skills.ReviewConfig{Enabled: true, Prompt: "Extract source credibility."},
	}}
	got := Merge([]NamedConfig{a, b})
	assert.True(t, got.ReviewEnabled)
	assert.Contains(t, got.ReviewPrompt, "## Skill: hugr-data")
	assert.Contains(t, got.ReviewPrompt, "## Skill: web")
}

func TestMerge_CompactionHintsUnioned(t *testing.T) {
	a := NamedConfig{Name: "hugr-data", Config: &skills.SkillMemoryConfig{
		Compaction: skills.CompactionHints{
			Preserve: []string{"schema", "numbers"},
			Discard:  []string{"greetings"},
		},
	}}
	b := NamedConfig{Name: "web", Config: &skills.SkillMemoryConfig{
		Compaction: skills.CompactionHints{
			Preserve: []string{"numbers", "citations"},
			Discard:  []string{"greetings", "filler"},
		},
	}}
	got := Merge([]NamedConfig{a, b})
	assert.ElementsMatch(t, []string{"schema", "numbers", "citations"}, got.CompactPreserve)
	assert.ElementsMatch(t, []string{"greetings", "filler"}, got.CompactDiscard)
}

func TestMerge_MinToolCallsMostRestrictive(t *testing.T) {
	a := NamedConfig{Name: "a", Config: &skills.SkillMemoryConfig{
		Review: skills.ReviewConfig{Enabled: true, MinToolCalls: 2},
	}}
	b := NamedConfig{Name: "b", Config: &skills.SkillMemoryConfig{
		Review: skills.ReviewConfig{Enabled: true, MinToolCalls: 5},
	}}
	got := Merge([]NamedConfig{a, b})
	assert.Equal(t, 5, got.MinToolCalls, "most-restrictive MinToolCalls wins")
}

func TestMerge_NilConfigSkipped(t *testing.T) {
	a := NamedConfig{Name: "no-memory", Config: nil}
	b := NamedConfig{Name: "has-memory", Config: &skills.SkillMemoryConfig{
		Categories: map[string]skills.CategoryConfig{"schema": {Volatility: "stable"}},
	}}
	got := Merge([]NamedConfig{a, b})
	assert.Contains(t, got.Categories, "has-memory.schema")
	assert.NotContains(t, got.Categories, "no-memory.schema")
}

func TestMerge_WindowAggregates(t *testing.T) {
	a := NamedConfig{Name: "a", Config: &skills.SkillMemoryConfig{
		Review: skills.ReviewConfig{
			WindowTokens:  8000,
			OverlapTokens: 400,
			FloorAge:      2 * time.Hour,
		},
	}}
	b := NamedConfig{Name: "b", Config: &skills.SkillMemoryConfig{
		Review: skills.ReviewConfig{
			WindowTokens:  4000, // smaller wins
			OverlapTokens: 800,  // larger wins
			FloorAge:      30 * time.Minute,
		},
	}}
	got := Merge([]NamedConfig{a, b})
	assert.Equal(t, 4000, got.WindowTokens, "MIN window_tokens")
	assert.Equal(t, 800, got.OverlapTokens, "MAX overlap_tokens")
	assert.Equal(t, 30*time.Minute, got.FloorAge, "MIN floor_age")
}

func TestMerge_ExcludeEventTypesUnion(t *testing.T) {
	a := NamedConfig{Name: "a", Config: &skills.SkillMemoryConfig{
		Review: skills.ReviewConfig{ExcludeEventTypes: []string{"compaction_summary", "reasoning"}},
	}}
	b := NamedConfig{Name: "b", Config: &skills.SkillMemoryConfig{
		Review: skills.ReviewConfig{ExcludeEventTypes: []string{"reasoning", "error"}},
	}}
	got := Merge([]NamedConfig{a, b})
	assert.Contains(t, got.ExcludeEventTypes, "compaction_summary")
	assert.Contains(t, got.ExcludeEventTypes, "reasoning")
	assert.Contains(t, got.ExcludeEventTypes, "error")
	assert.Len(t, got.ExcludeEventTypes, 3)
}
