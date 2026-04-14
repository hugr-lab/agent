package system

import (
	"log/slog"
	"net/http"
	"testing"

	testadapters "github.com/hugr-lab/hugen/adapters/test"
	"github.com/hugr-lab/hugen/interfaces"
	hugen "github.com/hugr-lab/hugen/pkg/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testDeps(skills interfaces.SkillProvider) *Deps {
	return &Deps{
		Skills:    skills,
		Prompt:    hugen.NewPromptBuilder("Base."),
		Toolset:   hugen.NewDynamicToolset(),
		Tokens:    hugen.NewTokenEstimator(),
		Transport: http.DefaultTransport,
		Logger:    slog.Default(),
	}
}

func TestSystemToolset_HasAllTools(t *testing.T) {
	sp := testadapters.NewStaticSkillProvider(nil)
	deps := testDeps(sp)

	ts := NewSystemToolset(deps)
	assert.Equal(t, "system", ts.Name())

	tools, err := ts.Tools(nil)
	require.NoError(t, err)
	assert.Len(t, tools, 4)

	names := make(map[string]bool)
	for _, tool := range tools {
		names[tool.Name()] = true
	}
	assert.True(t, names["skill_list"])
	assert.True(t, names["skill_load"])
	assert.True(t, names["skill_ref"])
	assert.True(t, names["context_status"])
}

func TestToolDeclarations(t *testing.T) {
	sp := testadapters.NewStaticSkillProvider(nil)
	deps := testDeps(sp)

	tests := []struct {
		name   string
		tool   interface{ Declaration() *interface{} }
		params []string
	}{
		{"skill_list", nil, nil},
		{"skill_load", nil, []string{"name"}},
		{"skill_ref", nil, []string{"skill", "ref"}},
	}

	tools, _ := NewSystemToolset(deps).Tools(nil)
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := tools[i]
			assert.Equal(t, tt.name, tool.Name())
			assert.NotEmpty(t, tool.Description())
			assert.False(t, tool.IsLongRunning())
		})
	}
}

func TestSkillListDeclaration(t *testing.T) {
	deps := testDeps(testadapters.NewStaticSkillProvider(nil))
	tool := &skillListTool{deps: deps}
	decl := tool.Declaration()
	assert.Equal(t, "skill_list", decl.Name)
}

func TestSkillLoadDeclaration(t *testing.T) {
	deps := testDeps(testadapters.NewStaticSkillProvider(nil))
	tool := &skillLoadTool{deps: deps}
	decl := tool.Declaration()
	assert.Equal(t, "skill_load", decl.Name)
	assert.Contains(t, decl.Parameters.Required, "name")
}

func TestSkillRefDeclaration(t *testing.T) {
	deps := testDeps(testadapters.NewStaticSkillProvider(nil))
	tool := &skillRefTool{deps: deps}
	decl := tool.Declaration()
	assert.Equal(t, "skill_ref", decl.Name)
	assert.Contains(t, decl.Parameters.Required, "skill")
	assert.Contains(t, decl.Parameters.Required, "ref")
}
