package learning

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMerge_CategoriesFirstWins(t *testing.T) {
	a := NamedConfig{Name: "hugr-data", Config: &SkillMemoryConfig{
		Categories: map[string]CategoryConfig{
			"schema": {Volatility: "stable", InitialScore: 0.8},
		},
	}}
	b := NamedConfig{Name: "web", Config: &SkillMemoryConfig{
		Categories: map[string]CategoryConfig{
			"schema": {Volatility: "volatile", InitialScore: 0.2},
			"extra":  {Volatility: "slow", InitialScore: 0.6},
		},
	}}
	got := Merge([]NamedConfig{a, b})
	assert.Equal(t, "stable", got.Categories["schema"].Volatility, "first-loaded wins")
	assert.Equal(t, 0.6, got.Categories["extra"].InitialScore)
}

func TestMergeWithLogger_EmitsCollisionWarning(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	a := NamedConfig{Name: "hugr-data", Config: &SkillMemoryConfig{
		Categories: map[string]CategoryConfig{"schema": {Volatility: "stable"}},
	}}
	b := NamedConfig{Name: "web", Config: &SkillMemoryConfig{
		Categories: map[string]CategoryConfig{"schema": {Volatility: "volatile"}},
	}}
	MergeWithLogger([]NamedConfig{a, b}, logger)
	out := buf.String()
	assert.Contains(t, out, "category collision")
	assert.Contains(t, out, "hugr-data")
	assert.Contains(t, out, "web")
}

func TestMerge_ReviewPromptsConcatenated(t *testing.T) {
	a := NamedConfig{Name: "hugr-data", Config: &SkillMemoryConfig{
		Review: ReviewConfig{Enabled: true, Prompt: "Extract schema facts."},
	}}
	b := NamedConfig{Name: "web", Config: &SkillMemoryConfig{
		Review: ReviewConfig{Enabled: true, Prompt: "Extract source credibility."},
	}}
	got := Merge([]NamedConfig{a, b})
	assert.True(t, got.ReviewEnabled)
	assert.Contains(t, got.ReviewPrompt, "## Skill: hugr-data")
	assert.Contains(t, got.ReviewPrompt, "## Skill: web")
}

func TestMerge_CompactionHintsUnioned(t *testing.T) {
	a := NamedConfig{Name: "hugr-data", Config: &SkillMemoryConfig{
		Compaction: CompactionHints{
			Preserve: []string{"schema", "numbers"},
			Discard:  []string{"greetings"},
		},
	}}
	b := NamedConfig{Name: "web", Config: &SkillMemoryConfig{
		Compaction: CompactionHints{
			Preserve: []string{"numbers", "citations"},
			Discard:  []string{"greetings", "filler"},
		},
	}}
	got := Merge([]NamedConfig{a, b})
	assert.ElementsMatch(t, []string{"schema", "numbers", "citations"}, got.CompactPreserve)
	assert.ElementsMatch(t, []string{"greetings", "filler"}, got.CompactDiscard)
}

func TestMerge_MinToolCallsMostRestrictive(t *testing.T) {
	a := NamedConfig{Name: "a", Config: &SkillMemoryConfig{
		Review: ReviewConfig{Enabled: true, MinToolCalls: 2},
	}}
	b := NamedConfig{Name: "b", Config: &SkillMemoryConfig{
		Review: ReviewConfig{Enabled: true, MinToolCalls: 5},
	}}
	got := Merge([]NamedConfig{a, b})
	assert.Equal(t, 5, got.MinToolCalls, "most-restrictive MinToolCalls wins")
}

func TestMerge_NilConfigSkipped(t *testing.T) {
	a := NamedConfig{Name: "no-memory", Config: nil}
	b := NamedConfig{Name: "has-memory", Config: &SkillMemoryConfig{
		Categories: map[string]CategoryConfig{"schema": {Volatility: "stable"}},
	}}
	got := Merge([]NamedConfig{a, b})
	assert.Contains(t, got.Categories, "schema")
}
