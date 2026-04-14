package agent

import (
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/interfaces"
	"github.com/stretchr/testify/assert"
)

const testSession = "test-session-1"

func TestPromptBuilder_Basic(t *testing.T) {
	pb := NewPromptBuilder("You are a test agent.")

	got := pb.BuildForSession(testSession)
	assert.Equal(t, "You are a test agent.", got)
}

func TestPromptBuilder_WithDefaultCatalog(t *testing.T) {
	pb := NewPromptBuilder("Base instructions.")
	pb.SetDefaultCatalog([]interfaces.SkillMeta{
		{Name: "hugr-data", Description: "Explore data", Categories: []string{"data"}},
		{Name: "search", Description: "Web search"},
	})

	// New sessions see the default catalog.
	got := pb.BuildForSession(testSession)
	assert.Contains(t, got, "Base instructions.")
	assert.Contains(t, got, "**hugr-data**: Explore data [data]")
	assert.Contains(t, got, "**search**: Web search")
}

func TestPromptBuilder_SessionCatalog(t *testing.T) {
	pb := NewPromptBuilder("Base.")
	pb.SetDefaultCatalog([]interfaces.SkillMeta{
		{Name: "default-skill", Description: "Default"},
	})

	// Session-specific catalog overrides default.
	pb.SetCatalog(testSession, []interfaces.SkillMeta{
		{Name: "session-skill", Description: "Session only"},
	})
	got := pb.BuildForSession(testSession)
	assert.Contains(t, got, "session-skill")
	assert.NotContains(t, got, "default-skill")

	// Other sessions still see default.
	other := pb.BuildForSession("other-session")
	assert.Contains(t, other, "default-skill")
	assert.NotContains(t, other, "session-skill")
}

func TestPromptBuilder_ClearCatalog(t *testing.T) {
	pb := NewPromptBuilder("Base.")
	pb.SetDefaultCatalog([]interfaces.SkillMeta{
		{Name: "s1", Description: "Skill 1"},
	})
	assert.Contains(t, pb.BuildForSession(testSession), "s1")

	pb.ClearCatalog(testSession)
	assert.NotContains(t, pb.BuildForSession(testSession), "s1")

	// Default catalog still visible to other sessions.
	assert.Contains(t, pb.BuildForSession("other-session"), "s1")
}

func TestPromptBuilder_SkillInstructions(t *testing.T) {
	pb := NewPromptBuilder("Base.")
	pb.SetSkillInstructions(testSession, "hugr-data", "# Hugr Data\nYou have data access.")

	got := pb.BuildForSession(testSession)
	assert.Contains(t, got, "# Hugr Data")

	// Other session doesn't see the skill.
	other := pb.BuildForSession("other-session")
	assert.NotContains(t, other, "# Hugr Data")
}

func TestPromptBuilder_ActiveSkill(t *testing.T) {
	pb := NewPromptBuilder("Base.")

	assert.Equal(t, "", pb.ActiveSkill(testSession))

	pb.SetSkillInstructions(testSession, "hugr-data", "Instructions.")
	assert.Equal(t, "hugr-data", pb.ActiveSkill(testSession))
	assert.Equal(t, "", pb.ActiveSkill("other-session"))
}

func TestPromptBuilder_References(t *testing.T) {
	pb := NewPromptBuilder("Base.")
	pb.SetSkillInstructions(testSession, "skill", "Skill instructions.")
	pb.AppendReference(testSession, "filters", "## Filters\n_eq, _neq, _gt")

	got := pb.BuildForSession(testSession)
	assert.Contains(t, got, "## Reference: filters")
	assert.Contains(t, got, "_eq, _neq, _gt")

	// Other session doesn't see references.
	other := pb.BuildForSession("other-session")
	assert.NotContains(t, other, "filters")
}

func TestPromptBuilder_ClearSkill(t *testing.T) {
	pb := NewPromptBuilder("Base.")
	pb.SetSkillInstructions(testSession, "skill", "Skill text.")
	pb.AppendReference(testSession, "ref1", "Reference content.")

	pb.ClearSkill(testSession)
	got := pb.BuildForSession(testSession)
	assert.Equal(t, "Base.", got)
	assert.Equal(t, "", pb.ActiveSkill(testSession))
}

func TestPromptBuilder_CharCount(t *testing.T) {
	pb := NewPromptBuilder("Base.")
	assert.Equal(t, len("Base."), pb.CharCountForSession(testSession))

	pb.SetSkillInstructions(testSession, "skill", strings.Repeat("x", 100))
	assert.Greater(t, pb.CharCountForSession(testSession), 100)
}

func TestPromptBuilder_SessionIsolation(t *testing.T) {
	pb := NewPromptBuilder("Constitution.")

	// Session A loads skill with references.
	pb.SetSkillInstructions("session-A", "data-skill", "Data instructions.")
	pb.AppendReference("session-A", "schema", "Schema docs.")
	pb.ClearCatalog("session-A")

	// Session B loads different skill.
	pb.SetSkillInstructions("session-B", "search-skill", "Search instructions.")
	pb.ClearCatalog("session-B")

	// Verify isolation.
	gotA := pb.BuildForSession("session-A")
	assert.Contains(t, gotA, "Data instructions.")
	assert.Contains(t, gotA, "Schema docs.")
	assert.NotContains(t, gotA, "Search instructions.")

	gotB := pb.BuildForSession("session-B")
	assert.Contains(t, gotB, "Search instructions.")
	assert.NotContains(t, gotB, "Data instructions.")
	assert.NotContains(t, gotB, "Schema docs.")
}

func TestPromptBuilder_CleanupSession(t *testing.T) {
	pb := NewPromptBuilder("Base.")
	pb.SetDefaultCatalog([]interfaces.SkillMeta{
		{Name: "default", Description: "Default skill"},
	})

	pb.SetSkillInstructions(testSession, "skill", "Loaded skill.")
	pb.ClearCatalog(testSession)

	// Cleanup resets session to see default catalog again.
	pb.CleanupSession(testSession)

	got := pb.BuildForSession(testSession)
	assert.NotContains(t, got, "Loaded skill.")
	assert.Contains(t, got, "default") // sees default catalog again
}
