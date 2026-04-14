package agent

import (
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/interfaces"
	"github.com/stretchr/testify/assert"
)

func TestPromptBuilder_Basic(t *testing.T) {
	pb := NewPromptBuilder("You are a test agent.")

	got := pb.Build()
	assert.Equal(t, "You are a test agent.", got)
}

func TestPromptBuilder_WithCatalog(t *testing.T) {
	pb := NewPromptBuilder("Base instructions.")
	pb.SetCatalog([]interfaces.SkillMeta{
		{Name: "hugr-data", Description: "Explore data", Categories: []string{"data"}},
		{Name: "search", Description: "Web search"},
	})

	got := pb.Build()
	assert.Contains(t, got, "Base instructions.")
	assert.Contains(t, got, "**hugr-data**: Explore data [data]")
	assert.Contains(t, got, "**search**: Web search")
}

func TestPromptBuilder_ClearCatalog(t *testing.T) {
	pb := NewPromptBuilder("Base.")
	pb.SetCatalog([]interfaces.SkillMeta{
		{Name: "s1", Description: "Skill 1"},
	})
	assert.Contains(t, pb.Build(), "s1")

	pb.ClearCatalog()
	assert.NotContains(t, pb.Build(), "s1")
}

func TestPromptBuilder_SkillInstructions(t *testing.T) {
	pb := NewPromptBuilder("Base.")
	pb.SetSkillInstructions("# Hugr Data\nYou have data access.")

	got := pb.Build()
	assert.Contains(t, got, "# Hugr Data")
}

func TestPromptBuilder_References(t *testing.T) {
	pb := NewPromptBuilder("Base.")
	pb.SetSkillInstructions("Skill instructions.")
	pb.AppendReference("filters", "## Filters\n_eq, _neq, _gt")

	got := pb.Build()
	assert.Contains(t, got, "## Reference: filters")
	assert.Contains(t, got, "_eq, _neq, _gt")
}

func TestPromptBuilder_ClearSkill(t *testing.T) {
	pb := NewPromptBuilder("Base.")
	pb.SetSkillInstructions("Skill text.")
	pb.AppendReference("ref1", "Reference content.")

	pb.ClearSkill()
	got := pb.Build()
	assert.Equal(t, "Base.", got)
}

func TestPromptBuilder_CharCount(t *testing.T) {
	pb := NewPromptBuilder("Base.")
	assert.Equal(t, len("Base."), pb.CharCount())

	pb.SetSkillInstructions(strings.Repeat("x", 100))
	assert.Greater(t, pb.CharCount(), 100)
}
