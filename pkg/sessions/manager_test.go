package sessions

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hugr-lab/hugen/pkg/skills"
	"github.com/hugr-lab/hugen/pkg/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	adksession "google.golang.org/adk/session"
	"google.golang.org/adk/tool"
)

func makeSkillsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	// demo skill: references the "hugr-main" provider we register in tm.
	demoDir := filepath.Join(dir, "demo")
	require.NoError(t, os.MkdirAll(demoDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(demoDir, "SKILL.md"), []byte(`---
name: demo
description: A demo skill
providers:
  - provider: hugr-main
---
Body.`), 0o644))

	// autoloaded system-ish skill: pulls in the "_skills" provider on
	// every session. Models how `_system` works in production.
	sysDir := filepath.Join(dir, "_sys")
	require.NoError(t, os.MkdirAll(sysDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sysDir, "SKILL.md"), []byte(`---
name: _sys
description: Autoload test suite
autoload: true
providers:
  - provider: _skills
---
system bootstrap text`), 0o644))

	return dir
}

// testHarness bundles the fixtures every test needs.
type testHarness struct {
	m     *Manager
	tools *tools.Manager
}

func newTestHarness(t *testing.T) *testHarness {
	t.Helper()
	sk, err := skills.NewFileManager(makeSkillsDir(t))
	require.NoError(t, err)

	tm := tools.New(nil)
	tm.AddProvider(tools.FakeProvider{N: "hugr-main", T: tools.FakeTools("demo_query", "demo_list")})
	tm.AddProvider(tools.FakeProvider{N: "_skills", T: tools.FakeTools("skill_list", "skill_load")})

	m := New(Config{
		Skills:       sk,
		Tools:        tm,
		Constitution: "C",
	})
	return &testHarness{m: m, tools: tm}
}

func TestManager_CreateGetDelete(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	resp, err := h.m.Create(ctx, &adksession.CreateRequest{AppName: "a", UserID: "u", SessionID: "s1"})
	require.NoError(t, err)
	require.NotNil(t, resp.Session)
	assert.Equal(t, "s1", resp.Session.ID())

	got, err := h.m.Get(ctx, &adksession.GetRequest{AppName: "a", UserID: "u", SessionID: "s1"})
	require.NoError(t, err)
	assert.Equal(t, "s1", got.Session.ID())

	runtimeSess, err := h.m.Session("s1")
	require.NoError(t, err)
	assert.Equal(t, "s1", runtimeSess.ID())

	require.NoError(t, h.m.Delete(ctx, &adksession.DeleteRequest{AppName: "a", UserID: "u", SessionID: "s1"}))
	_, err = h.m.Session("s1")
	assert.Error(t, err)
}

func TestManager_Create_AutoloadsSystemSkill(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	resp, err := h.m.Create(ctx, &adksession.CreateRequest{AppName: "a", UserID: "u", SessionID: "s1"})
	require.NoError(t, err)
	sess := resp.Session.(*Session)

	// Autoload should have fired `_sys` → skill_list, skill_load tools
	// are visible via the _skills provider binding.
	snap := sess.Snapshot()
	names := toolNames(snap.Tools)
	assert.Contains(t, names, "skill_list")
	assert.Contains(t, names, "skill_load")
	assert.Contains(t, snap.Prompt, "## Skill: _sys")
}

func TestSession_LoadSkill_Snapshot(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	resp, err := h.m.Create(ctx, &adksession.CreateRequest{AppName: "a", UserID: "u", SessionID: "s1"})
	require.NoError(t, err)
	sess := resp.Session.(*Session)

	require.NoError(t, sess.LoadSkill(ctx, "demo"))

	snap := sess.Snapshot()
	assert.Contains(t, snap.Prompt, "## Skill: demo")
	assert.Contains(t, snap.Prompt, "`demo_query`")

	names := toolNames(snap.Tools)
	// Autoloaded _sys system tools + demo's filtered view over hugr-main.
	assert.Contains(t, names, "skill_list")
	assert.Contains(t, names, "demo_query")
	assert.Contains(t, names, "demo_list")

	// Unknown skill errors cleanly.
	assert.Error(t, sess.LoadSkill(ctx, "ghost"))
}

func TestSession_UnloadSkill_DropsBindings(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	resp, _ := h.m.Create(ctx, &adksession.CreateRequest{AppName: "a", UserID: "u", SessionID: "s1"})
	sess := resp.Session.(*Session)
	require.NoError(t, sess.LoadSkill(ctx, "demo"))

	require.NoError(t, sess.UnloadSkill(ctx, "demo"))
	snap := sess.Snapshot()
	names := toolNames(snap.Tools)
	assert.NotContains(t, names, "demo_query")
	// system-suite tools still present (autoloaded, not unloaded).
	assert.Contains(t, names, "skill_list")

	_, err := h.tools.Provider("skill/demo/0")
	assert.Error(t, err, "binding view should be gone from tools.Manager")
}

func TestSession_SetCatalog_Clears(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()
	resp, _ := h.m.Create(ctx, &adksession.CreateRequest{AppName: "a", UserID: "u", SessionID: "s1"})
	sess := resp.Session.(*Session)

	require.NoError(t, sess.SetCatalog(nil))
	snap := sess.Snapshot()
	assert.NotContains(t, snap.Prompt, "Available Skills")
}

func toolNames(tools []tool.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name())
	}
	return out
}
