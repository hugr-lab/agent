package skills

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hugr-lab/hugen/interfaces"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupCatalogue(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	// test-skill: references a named provider + inline fallback — exercises
	// both branches of the providers: frontmatter list.
	skillDir := filepath.Join(dir, "test-skill")
	require.NoError(t, os.MkdirAll(filepath.Join(skillDir, "references"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(`---
name: test-skill
description: A test skill
categories: [test, unit]
autoload: false
providers:
  - provider: hugr-main
    tools: [discovery-*]
  - endpoint: http://localhost:8080/mcp
    tools: [x]
---

# Core instructions

Do things.
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "references", "filters.md"), []byte("# Filters\n\n_eq, _gt\n"), 0o644))

	// simple-skill: no providers, no mcp, autoload=false.
	simpleDir := filepath.Join(dir, "simple-skill")
	require.NoError(t, os.MkdirAll(simpleDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(simpleDir, "SKILL.md"), []byte(`---
name: simple-skill
description: Simple
---

Just text.
`), 0o644))

	// autoloaded-skill: verifies AutoloadNames picks it up.
	autoDir := filepath.Join(dir, "autoloaded-skill")
	require.NoError(t, os.MkdirAll(autoDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(autoDir, "SKILL.md"), []byte(`---
name: autoloaded-skill
description: Runs on every session
autoload: true
---

hi
`), 0o644))

	return dir
}

func TestFileManager_List(t *testing.T) {
	m, err := NewFileManager(setupCatalogue(t))
	require.NoError(t, err)

	metas, err := m.List(context.Background())
	require.NoError(t, err)
	assert.Len(t, metas, 3)

	names := map[string]bool{}
	for _, s := range metas {
		names[s.Name] = true
	}
	assert.True(t, names["test-skill"])
	assert.True(t, names["simple-skill"])
	assert.True(t, names["autoloaded-skill"])
}

func TestFileManager_Load_Providers(t *testing.T) {
	m, err := NewFileManager(setupCatalogue(t))
	require.NoError(t, err)

	s, err := m.Load(context.Background(), "test-skill")
	require.NoError(t, err)
	assert.Equal(t, "test-skill", s.Name)
	assert.False(t, s.Autoload)
	assert.Contains(t, s.Instructions, "Core instructions")
	assert.Len(t, s.Refs, 1)

	require.Len(t, s.Providers, 2)
	assert.Equal(t, "hugr-main", s.Providers[0].Provider)
	assert.Equal(t, []string{"discovery-*"}, s.Providers[0].Tools)
	assert.Equal(t, "http://localhost:8080/mcp", s.Providers[1].Endpoint)

	_, err = m.Load(context.Background(), "nope")
	assert.Error(t, err)
}

func TestFileManager_AutoloadNames(t *testing.T) {
	m, err := NewFileManager(setupCatalogue(t))
	require.NoError(t, err)

	names, err := m.AutoloadNames(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"autoloaded-skill"}, names)
}

func TestFileManager_Reference(t *testing.T) {
	m, err := NewFileManager(setupCatalogue(t))
	require.NoError(t, err)

	content, err := m.Reference(context.Background(), "test-skill", "filters")
	require.NoError(t, err)
	assert.Contains(t, content, "# Filters")

	_, err = m.Reference(context.Background(), "test-skill", "nope")
	assert.Error(t, err)
}

func TestFileManager_HotEdit(t *testing.T) {
	dir := setupCatalogue(t)
	m, err := NewFileManager(dir)
	require.NoError(t, err)

	metas, _ := m.List(context.Background())
	require.Len(t, metas, 3)

	// Add a fourth skill on disk — next List picks it up (no cache).
	newDir := filepath.Join(dir, "fresh-skill")
	require.NoError(t, os.MkdirAll(newDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(newDir, "SKILL.md"),
		[]byte("---\nname: fresh-skill\ndescription: Added live\n---\nHi"),
		0o644))

	metas, _ = m.List(context.Background())
	assert.Len(t, metas, 4)
}

func TestFileManager_RenderCatalog(t *testing.T) {
	m, err := NewFileManager(setupCatalogue(t))
	require.NoError(t, err)

	metas, _ := m.List(context.Background())
	text := m.RenderCatalog(metas)
	assert.Contains(t, text, "## Available Skills")
	assert.Contains(t, text, "test-skill")
	assert.Contains(t, text, "simple-skill")

	assert.Empty(t, m.RenderCatalog([]interfaces.SkillMeta{}))
}

func TestFileManager_EndpointEnvExpansion(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "env-skill")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: env-skill\nproviders:\n  - endpoint: ${TEST_MCP_EP}\n---\ntext"), 0o644))
	t.Setenv("TEST_MCP_EP", "http://test:9090/mcp")

	m, err := NewFileManager(dir)
	require.NoError(t, err)
	s, err := m.Load(context.Background(), "env-skill")
	require.NoError(t, err)
	require.Len(t, s.Providers, 1)
	assert.Equal(t, "http://test:9090/mcp", s.Providers[0].Endpoint)
}
