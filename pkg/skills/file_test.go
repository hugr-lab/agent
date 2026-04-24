package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeSkill is a helper that writes one skill directory containing a
// SKILL.md with the supplied frontmatter + body. Returns the parent
// directory so the caller can construct a fileManager rooted there.
func writeSkill(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	sd := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(sd, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sd, "SKILL.md"), []byte(body), 0o644))
	return dir
}

// ----- spec 006 sub_agents parsing -----

func TestLoad_SubAgents_Valid(t *testing.T) {
	dir := writeSkill(t, "hugr-data", `---
name: hugr-data
version: "0.1.0"
description: Hugr data exploration
autoload: false
providers:
  - name: hugr
    provider: hugr-main
sub_agents:
  schema_explorer:
    description: Discovers schemas of one module.
    intent: tool_calling
    tools: [hugr]
    tool_allowlist: [discovery-*, schema-*]
    max_turns: 12
    summary_max_tokens: 600
    instructions: |
      You are a schema explorer.
      Investigate one module thoroughly.
---

# Body
`)

	mgr, err := NewFileManager(dir)
	require.NoError(t, err)
	sk, err := mgr.Load(context.Background(), "hugr-data")
	require.NoError(t, err)
	require.NotNil(t, sk.SubAgents)

	role, ok := sk.SubAgents["schema_explorer"]
	require.True(t, ok)
	assert.Equal(t, "tool_calling", role.Intent)
	assert.Equal(t, []string{"hugr"}, role.Tools)
	assert.Equal(t, []string{"discovery-*", "schema-*"}, role.ToolAllowlist)
	assert.Equal(t, 12, role.MaxTurns)
	assert.Equal(t, 600, role.SummaryMaxTok)
	assert.Contains(t, role.Instructions, "schema explorer")
}

func TestLoad_SubAgents_Defaults(t *testing.T) {
	// Omitting max_turns / summary_max_tokens / intent must apply
	// defaults (15 / 800 / "" → router default model).
	dir := writeSkill(t, "hugr-data", `---
name: hugr-data
version: "0.1.0"
description: x
providers:
  - name: hugr
    provider: hugr-main
sub_agents:
  data_analyst:
    description: Analyses data.
    instructions: do analysis
---

# body
`)
	mgr, err := NewFileManager(dir)
	require.NoError(t, err)
	sk, err := mgr.Load(context.Background(), "hugr-data")
	require.NoError(t, err)

	role := sk.SubAgents["data_analyst"]
	assert.Equal(t, "", role.Intent, "intent default is empty (router maps to default model)")
	assert.Equal(t, defaultSubAgentMaxTurns, role.MaxTurns)
	assert.Equal(t, defaultSubAgentSummaryMaxTok, role.SummaryMaxTok)
	assert.Empty(t, role.Tools, "Tools omitted defaults to nil (= all providers)")
	assert.Empty(t, role.ToolAllowlist)
}

func TestLoad_SubAgents_RejectsUnknownTool(t *testing.T) {
	dir := writeSkill(t, "hugr-data", `---
name: hugr-data
version: "0.1.0"
description: x
providers:
  - name: hugr
    provider: hugr-main
sub_agents:
  bad_role:
    description: x
    instructions: x
    tools: [hugr, mystery]
---

# body
`)
	mgr, err := NewFileManager(dir)
	require.NoError(t, err)
	_, err = mgr.Load(context.Background(), "hugr-data")
	require.Error(t, err)
	msg := err.Error()
	assert.True(t, strings.Contains(msg, `"hugr-data"`), "error must name skill, got: %s", msg)
	assert.True(t, strings.Contains(msg, `"bad_role"`), "error must name role, got: %s", msg)
	assert.True(t, strings.Contains(msg, `"mystery"`), "error must name offending tool, got: %s", msg)
}

func TestLoad_SubAgents_RejectsEmptyInstructions(t *testing.T) {
	dir := writeSkill(t, "hugr-data", `---
name: hugr-data
version: "0.1.0"
description: x
providers:
  - name: hugr
    provider: hugr-main
sub_agents:
  empty_role:
    description: x
    instructions: ""
---

# body
`)
	mgr, err := NewFileManager(dir)
	require.NoError(t, err)
	_, err = mgr.Load(context.Background(), "hugr-data")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "instructions are required")
	assert.Contains(t, err.Error(), `"empty_role"`)
}

func TestLoad_SubAgents_RejectsNegativeCaps(t *testing.T) {
	cases := []struct {
		field   string
		value   string
		wantStr string
	}{
		{"max_turns", "max_turns: -3", "max_turns must be > 0"},
		{"summary_max_tokens", "summary_max_tokens: -100", "summary_max_tokens must be > 0"},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			dir := writeSkill(t, "hugr-data", `---
name: hugr-data
version: "0.1.0"
description: x
providers:
  - name: hugr
    provider: hugr-main
sub_agents:
  bad_role:
    description: x
    instructions: x
    `+tc.value+`
---

# body
`)
			mgr, err := NewFileManager(dir)
			require.NoError(t, err)
			_, err = mgr.Load(context.Background(), "hugr-data")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantStr)
		})
	}
}

func TestLoad_SubAgents_RejectsMissingDescription(t *testing.T) {
	dir := writeSkill(t, "hugr-data", `---
name: hugr-data
version: "0.1.0"
description: x
providers:
  - name: hugr
    provider: hugr-main
sub_agents:
  no_desc:
    instructions: do things
---

# body
`)
	mgr, err := NewFileManager(dir)
	require.NoError(t, err)
	_, err = mgr.Load(context.Background(), "hugr-data")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "description is required")
}

// ----- spec 006 autoload_for parsing -----

func TestLoad_AutoloadFor_DefaultsRootWhenAutoloadTrue(t *testing.T) {
	dir := writeSkill(t, "_memory", `---
name: _memory
version: "0.1.0"
description: x
autoload: true
---

# body
`)
	mgr, err := NewFileManager(dir)
	require.NoError(t, err)
	sk, err := mgr.Load(context.Background(), "_memory")
	require.NoError(t, err)
	assert.True(t, sk.Autoload)
	assert.Equal(t, []string{SessionTypeRoot}, sk.AutoloadFor)
}

func TestLoad_AutoloadFor_ExplicitList(t *testing.T) {
	dir := writeSkill(t, "_memory", `---
name: _memory
version: "0.1.0"
description: x
autoload: true
autoload_for: [root, subagent]
---

# body
`)
	mgr, err := NewFileManager(dir)
	require.NoError(t, err)
	sk, err := mgr.Load(context.Background(), "_memory")
	require.NoError(t, err)
	assert.True(t, sk.Autoload)
	assert.Equal(t, []string{SessionTypeRoot, SessionTypeSubAgent}, sk.AutoloadFor)
}

func TestLoad_AutoloadFor_EmptyWhenAutoloadFalse(t *testing.T) {
	// AutoloadFor only matters when Autoload is true; non-autoload
	// skills produce nil AutoloadFor regardless of declared list.
	dir := writeSkill(t, "hugr-data", `---
name: hugr-data
version: "0.1.0"
description: x
autoload: false
autoload_for: [subagent]
---

# body
`)
	mgr, err := NewFileManager(dir)
	require.NoError(t, err)
	sk, err := mgr.Load(context.Background(), "hugr-data")
	require.NoError(t, err)
	assert.False(t, sk.Autoload)
	assert.Nil(t, sk.AutoloadFor)
}

func TestLoad_AutoloadFor_DedupsBlanksAndDuplicates(t *testing.T) {
	dir := writeSkill(t, "_x", `---
name: _x
version: "0.1.0"
description: x
autoload: true
autoload_for: ["root", "", "subagent", "root"]
---

# body
`)
	mgr, err := NewFileManager(dir)
	require.NoError(t, err)
	sk, err := mgr.Load(context.Background(), "_x")
	require.NoError(t, err)
	assert.Equal(t, []string{SessionTypeRoot, SessionTypeSubAgent}, sk.AutoloadFor)
}
