package memory

import (
	"testing"

	"github.com/hugr-lab/hugen/pkg/skills"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseReviewOutput_Strict(t *testing.T) {
	raw := `{"facts": [{"content": "tf.incidents has 14 fields", "category": "schema", "volatility": "stable", "tags": ["tf","schema"]}], "hypotheses": []}`
	got, err := parseReviewOutput(raw)
	require.NoError(t, err)
	require.Len(t, got.Facts, 1)
	assert.Equal(t, "tf.incidents has 14 fields", got.Facts[0].Content)
	assert.Equal(t, "schema", got.Facts[0].Category)
	assert.ElementsMatch(t, []string{"tf", "schema"}, got.Facts[0].Tags)
}

func TestParseReviewOutput_CodeFence(t *testing.T) {
	raw := "```json\n" + `{"facts": [], "hypotheses": [{"content": "severity has 3 values", "priority": "high", "verification": "query distinct", "estimated_calls": 2}]}` + "\n```"
	got, err := parseReviewOutput(raw)
	require.NoError(t, err)
	require.Len(t, got.Hypotheses, 1)
	assert.Equal(t, "severity has 3 values", got.Hypotheses[0].Content)
}

func TestParseReviewOutput_NoJSON(t *testing.T) {
	_, err := parseReviewOutput("sorry, I can't help")
	require.Error(t, err)
}

func TestEqualEnough(t *testing.T) {
	assert.True(t, equalEnough("abc", "abc"))
	assert.True(t, equalEnough("ABC", "abc"))
	assert.True(t, equalEnough("the quick brown fox jumps over", "the quick brown fox jumps ov"))
	assert.False(t, equalEnough("abc", "xyz"))
	assert.False(t, equalEnough("", "abc"))
}

// TestReviewer_DefaultExcludeEventTypes_Phase4 asserts that the
// reviewer's hardcoded fallback list excludes all framework lifecycle
// audit events through phase 4 — no skill memory.yaml needs to repeat
// them. Spec 009 / US7 / T107.
func TestReviewer_DefaultExcludeEventTypes_Phase4(t *testing.T) {
	got := DefaultExcludeEventTypes()
	want := []string{
		// Transcript chrome
		"compaction_summary", "reasoning", "error",
		// Mission lifecycle (phase 2)
		"agent_spawn", "agent_result", "agent_abstained", "user_followup_routed",
		// Artifact lifecycle (phase 3)
		"artifact_published", "artifact_granted", "artifact_removed",
		// HITL / policy / composition lifecycle (phase 4)
		"approval_requested", "approval_responded", "policy_changed", "ask_coordinator",
	}
	for _, et := range want {
		assert.Contains(t, got, et, "default exclude list missing %q", et)
	}
}

// TestReviewer_SkillCanIncludeBackEventType asserts that a skill
// memory.yaml setting `review.include_event_types: [<x>]` overrides
// the default exclude — i.e. the skill pulls the event type back into
// its review window. Per-skill explicit excludes still win. T108.
func TestReviewer_SkillCanIncludeBackEventType(t *testing.T) {
	r := &Reviewer{
		defaultExcludeTypes: DefaultExcludeEventTypes(),
	}

	// Baseline: no skill includes any default-excluded type — full
	// default list is excluded.
	plain := r.excludeTypes(MergedConfig{
		ExcludeEventTypes: map[string]struct{}{},
		IncludeEventTypes: map[string]struct{}{},
	})
	assert.Contains(t, plain, "approval_responded")
	assert.Contains(t, plain, "agent_result")

	// One skill pulls approval_responded back via include_event_types:
	override := r.excludeTypes(MergedConfig{
		ExcludeEventTypes: map[string]struct{}{},
		IncludeEventTypes: map[string]struct{}{"approval_responded": {}},
	})
	assert.NotContains(t, override, "approval_responded",
		"include_event_types should override default exclude")
	// Other defaults unaffected.
	assert.Contains(t, override, "agent_result")

	// If a skill ALSO explicitly excludes the same type, exclude wins:
	conflict := r.excludeTypes(MergedConfig{
		ExcludeEventTypes: map[string]struct{}{"approval_responded": {}},
		IncludeEventTypes: map[string]struct{}{"approval_responded": {}},
	})
	assert.Contains(t, conflict, "approval_responded",
		"explicit exclude should win over include")
}

// TestReviewer_MergeIncludeEventTypes asserts that Merge collects
// IncludeEventTypes from multiple skills via union (any skill including
// a type pulls it back). T108 cross-skill leg.
func TestReviewer_MergeIncludeEventTypes(t *testing.T) {
	merged := Merge([]NamedConfig{
		{Name: "hugr-data", Config: &skills.SkillMemoryConfig{
			Review: skills.ReviewConfig{
				Enabled:           true,
				IncludeEventTypes: []string{"approval_responded"},
			},
		}},
		{Name: "_memory", Config: &skills.SkillMemoryConfig{
			Review: skills.ReviewConfig{
				Enabled:           true,
				IncludeEventTypes: []string{"agent_result"},
			},
		}},
	})
	assert.Contains(t, merged.IncludeEventTypes, "approval_responded")
	assert.Contains(t, merged.IncludeEventTypes, "agent_result")
}

func TestDurationFor_Defaults(t *testing.T) {
	r := &Reviewer{}
	assert.Equal(t, int64(24), int64(r.durationFor("volatile").Hours()))
	assert.Equal(t, int64(168), int64(r.durationFor("fast").Hours()))
	assert.Equal(t, int64(720), int64(r.durationFor("moderate").Hours()))
	assert.Equal(t, int64(2160), int64(r.durationFor("slow").Hours()))
	assert.Equal(t, int64(8760), int64(r.durationFor("stable").Hours()))
	assert.Equal(t, int64(8760), int64(r.durationFor("unknown").Hours()))
}
