package file

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestSkills(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	// Create a test skill.
	skillDir := filepath.Join(dir, "test-skill")
	require.NoError(t, os.MkdirAll(filepath.Join(skillDir, "references"), 0755))

	skillMD := `---
name: test-skill
description: A test skill for unit testing
categories: [test, unit]
---

# Test Skill

These are the core instructions for the test skill.
`
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillMD), 0644))

	mcpYAML := "endpoint: http://localhost:8080/mcp\n"
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "mcp.yaml"), []byte(mcpYAML), 0644))

	ref1 := "# Filters\n\n_eq, _neq, _gt operators."
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "references", "filters.md"), []byte(ref1), 0644))

	ref2 := "# Patterns\n\nQuery patterns doc."
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "references", "patterns.md"), []byte(ref2), 0644))

	// Create a skill without mcp.yaml.
	simpleDir := filepath.Join(dir, "simple-skill")
	require.NoError(t, os.MkdirAll(simpleDir, 0755))
	simpleMD := `---
name: simple-skill
description: Simple skill without MCP
---

Simple instructions.
`
	require.NoError(t, os.WriteFile(filepath.Join(simpleDir, "SKILL.md"), []byte(simpleMD), 0644))

	return dir
}

func TestSkillProvider_ListMeta(t *testing.T) {
	dir := setupTestSkills(t)
	sp := NewSkillProvider(dir)

	skills, err := sp.ListMeta(context.Background())
	require.NoError(t, err)
	assert.Len(t, skills, 2)

	names := make(map[string]bool)
	for _, s := range skills {
		names[s.Name] = true
	}
	assert.True(t, names["test-skill"])
	assert.True(t, names["simple-skill"])
}

func TestSkillProvider_LoadFull(t *testing.T) {
	dir := setupTestSkills(t)
	sp := NewSkillProvider(dir)

	skill, err := sp.LoadFull(context.Background(), "test-skill")
	require.NoError(t, err)

	assert.Equal(t, "test-skill", skill.Name)
	assert.Contains(t, skill.Instructions, "# Test Skill")
	assert.Contains(t, skill.Instructions, "core instructions")
	assert.Equal(t, "http://localhost:8080/mcp", skill.MCPEndpoint)

	// Should have 2 references.
	assert.Len(t, skill.References, 2)
	refNames := make(map[string]bool)
	for _, r := range skill.References {
		refNames[r.Name] = true
	}
	assert.True(t, refNames["filters"])
	assert.True(t, refNames["patterns"])
}

func TestSkillProvider_LoadFull_NoMCP(t *testing.T) {
	dir := setupTestSkills(t)
	sp := NewSkillProvider(dir)

	skill, err := sp.LoadFull(context.Background(), "simple-skill")
	require.NoError(t, err)
	assert.Equal(t, "simple-skill", skill.Name)
	assert.Empty(t, skill.MCPEndpoint)
}

func TestSkillProvider_LoadFull_NotFound(t *testing.T) {
	dir := setupTestSkills(t)
	sp := NewSkillProvider(dir)

	_, err := sp.LoadFull(context.Background(), "nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestSkillProvider_LoadRef(t *testing.T) {
	dir := setupTestSkills(t)
	sp := NewSkillProvider(dir)

	content, err := sp.LoadRef(context.Background(), "test-skill", "filters")
	require.NoError(t, err)
	assert.Contains(t, content, "# Filters")
	assert.Contains(t, content, "_eq")
}

func TestSkillProvider_LoadRef_NotFound(t *testing.T) {
	dir := setupTestSkills(t)
	sp := NewSkillProvider(dir)

	_, err := sp.LoadRef(context.Background(), "test-skill", "nonexistent")
	assert.Error(t, err)
}

func TestParseFrontmatter(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantName    string
		wantContent string
	}{
		{
			name:        "valid frontmatter",
			input:       "---\nname: test\ndescription: desc\n---\n# Content",
			wantName:    "test",
			wantContent: "# Content",
		},
		{
			name:        "no frontmatter",
			input:       "# Just content",
			wantName:    "",
			wantContent: "# Just content",
		},
		{
			name:        "empty content after frontmatter",
			input:       "---\nname: test\n---\n",
			wantName:    "test",
			wantContent: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fm, content := parseFrontmatter(tt.input)
			assert.Equal(t, tt.wantName, fm.Name)
			assert.Equal(t, tt.wantContent, content)
		})
	}
}

func TestSkillProvider_MCPEnvExpansion(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "env-skill")
	require.NoError(t, os.MkdirAll(skillDir, 0755))

	skillMD := "---\nname: env-skill\n---\nInstructions."
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillMD), 0644))

	mcpYAML := "endpoint: ${TEST_MCP_ENDPOINT}\n"
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "mcp.yaml"), []byte(mcpYAML), 0644))

	t.Setenv("TEST_MCP_ENDPOINT", "http://test:9090/mcp")

	sp := NewSkillProvider(dir)
	skill, err := sp.LoadFull(context.Background(), "env-skill")
	require.NoError(t, err)
	assert.Equal(t, "http://test:9090/mcp", skill.MCPEndpoint)
}
