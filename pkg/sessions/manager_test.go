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
version: "0.1.0"
description: A demo skill
providers:
  - name: hugr-main
    provider: hugr-main
---
Body.`), 0o644))

	// autoloaded system-ish skill: pulls in the "_skills" provider on
	// every session. Models how `_system` works in production.
	sysDir := filepath.Join(dir, "_sys")
	require.NoError(t, os.MkdirAll(sysDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sysDir, "SKILL.md"), []byte(`---
name: _sys
version: "0.1.0"
description: Autoload test suite
autoload: true
providers:
  - name: _skills
    provider: _skills
---
system bootstrap text`), 0o644))

	return dir
}

// testHarness bundles the fixtures every test needs.
type testHarness struct {
	m          *Manager
	tools      *tools.Manager
	skillsRoot string
}

func newTestHarness(t *testing.T) *testHarness {
	t.Helper()
	root := makeSkillsDir(t)
	sk, err := skills.NewFileManager(root)
	require.NoError(t, err)

	tm := tools.New(nil)
	tm.AddProvider(tools.FakeProvider{N: "hugr-main", T: tools.FakeTools("demo_query", "demo_list")})
	tm.AddProvider(tools.FakeProvider{N: "_skills", T: tools.FakeTools("skill_list", "skill_load")})

	m, err := New(Config{
		Skills:       sk,
		Tools:        tm,
		Constitution: "C",
	})
	require.NoError(t, err)
	return &testHarness{m: m, tools: tm, skillsRoot: root}
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

	_, err := h.tools.Provider("skill/demo/hugr-main")
	assert.Error(t, err, "binding view should be gone from tools.Manager")
}

// TestSession_LoadSkill_VersionDrift verifies that editing a skill's
// version on disk + re-loading drops the stale bindings and creates
// fresh ones pointing at the new provider set.
func TestSession_LoadSkill_VersionDrift(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	resp, _ := h.m.Create(ctx, &adksession.CreateRequest{AppName: "a", UserID: "u", SessionID: "s1"})
	sess := resp.Session.(*Session)

	require.NoError(t, sess.LoadSkill(ctx, "demo"))
	// Baseline: hugr-main tools exposed, no "alt" binding yet.
	names := toolNames(sess.Snapshot().Tools)
	assert.Contains(t, names, "demo_query")

	// Register a second upstream provider and rewrite the demo skill
	// on disk: version bumped, hugr-main replaced with alt-source.
	h.tools.AddProvider(tools.FakeProvider{N: "alt-source", T: tools.FakeTools("alt_probe")})
	require.NoError(t, os.WriteFile(filepath.Join(h.skillsRoot, "demo", "SKILL.md"), []byte(`---
name: demo
version: "0.2.0"
description: A demo skill
providers:
  - name: alt
    provider: alt-source
---
Body.`), 0o644))

	require.NoError(t, sess.LoadSkill(ctx, "demo"))

	names = toolNames(sess.Snapshot().Tools)
	assert.Contains(t, names, "alt_probe", "new provider's tools should be exposed after version drift")
	assert.NotContains(t, names, "demo_query", "old provider's tools should be gone after version drift")

	// The old filtered binding key is gone.
	_, err := h.tools.Provider("skill/demo/hugr-main")
	assert.Error(t, err, "stale version binding should be dropped")
	_, err = h.tools.Provider("skill/demo/alt")
	assert.NoError(t, err, "new version binding should be registered")
}

// TestSession_LoadSkill_SameVersionNoop verifies that re-loading a
// skill whose disk definition hasn't changed doesn't re-bind
// providers or double-log skill_loaded.
func TestSession_LoadSkill_SameVersionNoop(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()
	resp, _ := h.m.Create(ctx, &adksession.CreateRequest{AppName: "a", UserID: "u", SessionID: "s1"})
	sess := resp.Session.(*Session)

	require.NoError(t, sess.LoadSkill(ctx, "demo"))
	first, err := h.tools.Provider("skill/demo/hugr-main")
	require.NoError(t, err)

	// Second load on unchanged version: binding object must be the
	// same instance; bindSkillProvider short-circuits.
	require.NoError(t, sess.LoadSkill(ctx, "demo"))
	second, err := h.tools.Provider("skill/demo/hugr-main")
	require.NoError(t, err)
	assert.Same(t, first, second, "same-version re-load should not rebuild binding")
}

// TestSession_LoadSkill_PartialBindRollback verifies the rollback
// logic when one of several providers fails to bind: only the
// providers that were added *in this call* get removed, preserving
// anything already present from an earlier successful load.
func TestSession_LoadSkill_PartialBindRollback(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// Two-provider skill: p1 resolves fine; p2 references a missing
	// upstream provider so bindSkillProvider fails at index 1.
	root := h.skillsRoot
	require.NoError(t, os.MkdirAll(filepath.Join(root, "partial"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "partial", "SKILL.md"), []byte(`---
name: partial
version: "0.1.0"
providers:
  - name: ok
    provider: hugr-main
  - name: bad
    provider: ghost
---
body`), 0o644))

	resp, _ := h.m.Create(ctx, &adksession.CreateRequest{AppName: "a", UserID: "u", SessionID: "s1"})
	sess := resp.Session.(*Session)

	err := sess.LoadSkill(ctx, "partial")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bad")

	// Neither binding should remain — the first was added in this
	// call so it gets rolled back.
	_, err = h.tools.Provider("skill/partial/ok")
	assert.Error(t, err, "newly added binding rolled back on failure")
	_, err = h.tools.Provider("skill/partial/bad")
	assert.Error(t, err)
}

// TestSession_UnloadSkill_ForgetsVersion verifies that Unload clears
// the version record so the next Load doesn't skip the bind due to
// a stale version match.
func TestSession_UnloadSkill_ForgetsVersion(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()
	resp, _ := h.m.Create(ctx, &adksession.CreateRequest{AppName: "a", UserID: "u", SessionID: "s1"})
	sess := resp.Session.(*Session)

	require.NoError(t, sess.LoadSkill(ctx, "demo"))
	_, tracked := sess.state.LoadedSkillVersion("demo")
	assert.True(t, tracked)

	require.NoError(t, sess.UnloadSkill(ctx, "demo"))
	_, tracked = sess.state.LoadedSkillVersion("demo")
	assert.False(t, tracked, "UnloadSkill must clear SkillVersions[name]")

	// Re-load works and re-binds.
	require.NoError(t, sess.LoadSkill(ctx, "demo"))
	_, err := h.tools.Provider("skill/demo/hugr-main")
	require.NoError(t, err)
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

// ===== spec 006 — Create well-known State keys + autoload filter =====

// makeAutoloadCatalogue builds three skills exercising the
// autoload_for filter:
//   - "_root_only"  → autoload_for: [root]      (default behaviour)
//   - "_both"       → autoload_for: [root, subagent]
//   - "_sub_only"   → autoload_for: [subagent]
// Each registers its own provider name so we can verify the filter
// fired correctly by inspecting the session's tool list.
func makeAutoloadCatalogue(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	type skill struct {
		name        string
		autoloadFor string
		provider    string
	}
	skills := []skill{
		{"_root_only", "[root]", "_skills"},
		{"_both", "[root, subagent]", "_memory"},
		{"_sub_only", "[subagent]", "_context"},
	}
	for _, s := range skills {
		sd := filepath.Join(dir, s.name)
		require.NoError(t, os.MkdirAll(sd, 0o755))
		body := `---
name: ` + s.name + `
version: "0.1.0"
description: x
autoload: true
autoload_for: ` + s.autoloadFor + `
providers:
  - name: ` + s.provider + `
    provider: ` + s.provider + `
---
body
`
		require.NoError(t, os.WriteFile(filepath.Join(sd, "SKILL.md"), []byte(body), 0o644))
	}
	return dir
}

func newAutoloadHarness(t *testing.T) *testHarness {
	t.Helper()
	root := makeAutoloadCatalogue(t)
	sk, err := skills.NewFileManager(root)
	require.NoError(t, err)

	tm := tools.New(nil)
	tm.AddProvider(tools.FakeProvider{N: "_skills", T: tools.FakeTools("skill_list")})
	tm.AddProvider(tools.FakeProvider{N: "_memory", T: tools.FakeTools("memory_search")})
	tm.AddProvider(tools.FakeProvider{N: "_context", T: tools.FakeTools("context_status")})

	m, err := New(Config{Skills: sk, Tools: tm, Constitution: "C"})
	require.NoError(t, err)
	return &testHarness{m: m, tools: tm, skillsRoot: root}
}

// TestManager_Create_RootSession_Default — when CreateRequest carries
// none of the well-known State keys, the session is "root" and only
// autoload skills with "root" in their autoload_for fire.
func TestManager_Create_RootSession_Default(t *testing.T) {
	h := newAutoloadHarness(t)
	ctx := context.Background()

	resp, err := h.m.Create(ctx, &adksession.CreateRequest{
		AppName: "a", UserID: "u", SessionID: "s-root",
	})
	require.NoError(t, err)
	sess := resp.Session.(*Session)

	assert.Equal(t, "root", sess.SessionType())
	assert.Empty(t, sess.ParentSessionID())
	assert.Empty(t, sess.SpawnedFromEventID())
	assert.Empty(t, sess.Mission())

	snap := sess.Snapshot()
	names := toolNames(snap.Tools)
	assert.Contains(t, names, "skill_list", "_root_only should autoload on root session")
	assert.Contains(t, names, "memory_search", "_both should autoload on root session")
	assert.NotContains(t, names, "context_status", "_sub_only must NOT autoload on root session")
}

// TestManager_Create_SubAgentSession_StateKeys — exercising the five
// well-known CreateRequest.State keys: session type + linkage land on
// the Session, and applyAutoload only picks up skills whose
// autoload_for includes "subagent".
func TestManager_Create_SubAgentSession_StateKeys(t *testing.T) {
	h := newAutoloadHarness(t)
	ctx := context.Background()

	resp, err := h.m.Create(ctx, &adksession.CreateRequest{
		AppName: "a", UserID: "u", SessionID: "s-sub",
		State: map[string]any{
			"__session_type__":           "subagent",
			"__parent_session_id__":      "s-coord",
			"__spawned_from_event_id__":  "evt_dispatch",
			"__mission__":                "describe tf.incidents",
			// __fork_after_seq__ omitted on purpose — sub-agents have NULL
			// fork_after_seq.
			"some_user_state":            "value",
		},
	})
	require.NoError(t, err)
	sess := resp.Session.(*Session)

	assert.Equal(t, "subagent", sess.SessionType())
	assert.Equal(t, "s-coord", sess.ParentSessionID())
	assert.Equal(t, "evt_dispatch", sess.SpawnedFromEventID())
	assert.Equal(t, "describe tf.incidents", sess.Mission())

	// Well-known keys MUST NOT bleed into Session.state — they're typed
	// fields on the Session, not generic state. Non-reserved user state
	// keys still pass through verbatim.
	stateMap := map[string]any{}
	for k, v := range sess.state.All() {
		stateMap[k] = v
	}
	assert.NotContains(t, stateMap, "__session_type__")
	assert.NotContains(t, stateMap, "__parent_session_id__")
	assert.NotContains(t, stateMap, "__spawned_from_event_id__")
	assert.NotContains(t, stateMap, "__mission__")
	assert.Equal(t, "value", stateMap["some_user_state"])

	// Autoload filter: only _both + _sub_only fire (autoload_for
	// includes "subagent"); _root_only is skipped.
	snap := sess.Snapshot()
	names := toolNames(snap.Tools)
	assert.NotContains(t, names, "skill_list", "_root_only should NOT autoload on subagent session")
	assert.Contains(t, names, "memory_search", "_both should autoload on subagent session")
	assert.Contains(t, names, "context_status", "_sub_only should autoload on subagent session")
}

// TestManager_Create_ForkSession_PassesForkAfterSeq — fork sessions
// (future user-fork feature) carry both parent_session_id AND
// fork_after_seq. Verify the integer round-trips through State.
func TestManager_Create_ForkSession_PassesForkAfterSeq(t *testing.T) {
	h := newAutoloadHarness(t)
	ctx := context.Background()

	resp, err := h.m.Create(ctx, &adksession.CreateRequest{
		AppName: "a", UserID: "u", SessionID: "s-fork",
		State: map[string]any{
			"__session_type__":      "fork",
			"__parent_session_id__": "s-original",
			"__fork_after_seq__":    7, // user forked the conversation at seq 7
		},
	})
	require.NoError(t, err)
	sess := resp.Session.(*Session)

	assert.Equal(t, "fork", sess.SessionType())
	assert.Equal(t, "s-original", sess.ParentSessionID())
	require.NotNil(t, sess.forkAfterSeq, "ForkAfterSeq must be set when supplied")
	assert.Equal(t, 7, *sess.forkAfterSeq)
}

// TestManager_Create_SubAgentNoSkillCatalog — sub-agent sessions
// MUST NOT include the "Available Skills" catalog block in their
// Snapshot prompt (spec 006 §3a — sub-agent doesn't pick skills
// mid-mission). Phase-1 implementation point: the catalog is
// rendered conditionally; verify the gate works once Snapshot has
// the relevant guard. Until that lives in pkg/skills/render, this
// test documents the expectation as a TODO assertion.
//
// NOTE (phase 1, T105 deferred): Snapshot still always renders the
// catalog. This assertion is left commented in to lock the contract
// once the rendering tweak ships in Phase 3.
//
//	snap := sess.Snapshot()
//	assert.NotContains(t, snap.Prompt, "Available Skills",
//	    "sub-agent snapshot must omit the skill catalog (spec 006 §3a)")
func TestManager_Create_SubAgentNoSkillCatalog(t *testing.T) {
	t.Skip("Snapshot catalog gating moves to Phase 3 (T105) — see comment.")
}
