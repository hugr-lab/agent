//go:build duckdb_arrow && scenario

// Package harness spins up a full hugr-agent runtime for scenario
// runners under tests/scenarios/. Uses pkg/runtime.Build directly so
// the wiring matches production exactly — no drift between cmd/agent
// and the harness.
//
// Config sourcing: harness loads tests/scenarios/config.yaml (shape
// mirrors the production config.yaml but stripped to what tests need)
// through the same pkg/config.LoadLocal path production uses, after
// sourcing tests/scenarios/.test.env into the process environment.
// That file is gitignored — copy .test.env.example and fill in your
// LM Studio / remote Hugr URLs. Per-scenario overrides stack on top
// via <scenarioDir>/config_override.yaml (llm.* + chatcontext.* today).
//
// A scenario run is observational — no hard assertions. Output goes
// to t.Log and hub.db is preserved under tests/scenarios/.data/<name>
// -<ts>/ for follow-up DuckDB inspection.
package harness

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/hugr-lab/hugen/internal/testenv"
	"github.com/hugr-lab/hugen/pkg/config"
	hugenruntime "github.com/hugr-lab/hugen/pkg/runtime"

	qetypes "github.com/hugr-lab/query-engine/types"
)

// Opts tunes Setup. ScenarioName is the only required field.
type Opts struct {
	// ScenarioName names the hub.db artefact + the default session id
	// prefix. Required.
	ScenarioName string

	// ConfigOverridePath is a yaml file overlaid onto the baseline
	// config after it loads (typically <scenarioDir>/config_override.yaml).
	ConfigOverridePath string

	// PersistDB, when non-empty, overrides the hub.db artefact path.
	// Equivalent to env SCENARIO_PERSIST. Empty → tests/scenarios/.data/
	// <name>-<timestamp>/memory.db.
	PersistDB string
}

// Agent bundles everything scenarios need.
type Agent struct {
	Runtime *hugenruntime.Runtime
	AgentID string
	AppName string
	UserID  string
	HubPath string

	logger *slog.Logger
}

// Engine is a shortcut for callers that just want the Querier.
func (a *Agent) Engine() qetypes.Querier { return a.Runtime.Querier }

// Close delegates to Runtime.Close. Already wired as t.Cleanup by
// Setup — exposed for early-close callers.
func (a *Agent) Close() { a.Runtime.Close() }

// Setup builds the runtime and wires t.Cleanup. Skips when required
// env vars are missing.
func Setup(t *testing.T, o Opts) *Agent {
	t.Helper()
	require.NotEmpty(t, o.ScenarioName, "harness.Setup: ScenarioName required")

	// .test.env is the source of secrets / URLs for scenarios. Falls
	// back to repo-root .env when absent (useful locally — CI should
	// use a dedicated .test.env).
	loadTestEnv(t)

	testenv.EnvOrSkip(t, "LLM_LOCAL_URL")
	testenv.EnvOrSkip(t, "EMBED_LOCAL_URL")

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	// --- Paths ---
	dbPath := o.PersistDB
	if dbPath == "" {
		dbPath = os.Getenv("SCENARIO_PERSIST")
	}
	if dbPath == "" {
		runDir := filepath.Join(scenarioDataDir(),
			fmt.Sprintf("%s-%s", o.ScenarioName, time.Now().UTC().Format("20060102-150405")))
		require.NoError(t, os.MkdirAll(runDir, 0o755))
		dbPath = filepath.Join(runDir, "memory.db")
	} else {
		require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
	}
	coreDBDir := filepath.Join(filepath.Dir(dbPath), "core")
	require.NoError(t, os.MkdirAll(coreDBDir, 0o755))

	skillsPath := buildSkillsDir(t, fixtureSkillsPath())

	// --- Load tests/scenarios/config.yaml via the production path ---
	boot, err := config.LoadBootstrap("") // .env already applied; skip re-read
	require.NoError(t, err, "harness: bootstrap")

	cfgPath := filepath.Join(scenariosRoot(), "config.yaml")
	cfg, err := config.LoadLocal(cfgPath, boot)
	require.NoError(t, err, "harness: load config %s", cfgPath)

	// --- Override runtime-only fields the YAML can't know about ---
	cfg.LocalDB.DB.Path = filepath.Join(coreDBDir, "engine.db")
	cfg.LocalDB.DB.Settings.HomeDirectory = coreDBDir
	cfg.LocalDB.MemoryPath = dbPath
	cfg.Skills.Path = skillsPath
	if !filepath.IsAbs(cfg.Agent.Constitution) {
		cfg.Agent.Constitution = filepath.Join(repoRoot(), cfg.Agent.Constitution)
	}
	// Honour INTEGRATION_AGENT_MODEL for quick per-run model swaps.
	if m := os.Getenv("INTEGRATION_AGENT_MODEL"); m != "" {
		cfg.LLM.Model = m
		if cfg.LLM.Routes != nil {
			cfg.LLM.Routes["default"] = m
		}
	}

	// --- Apply per-scenario override ---
	if o.ConfigOverridePath != "" {
		require.NoError(t, applyConfigOverride(cfg, o.ConfigOverridePath))
	}

	// --- Build runtime ---
	ctx := context.Background()
	rt, err := hugenruntime.Build(ctx, cfg, logger, hugenruntime.Options{})
	require.NoError(t, err, "harness: runtime.Build")

	a := &Agent{
		Runtime: rt,
		AgentID: cfg.Identity.ID,
		AppName: "hugr_agent",
		UserID:  "scenario-user",
		HubPath: dbPath,
		logger:  logger,
	}
	t.Cleanup(func() {
		rt.Close()
		if os.Getenv("DROP_DB") == "1" {
			_ = os.Remove(dbPath)
			return
		}
		t.Logf("harness: hub.db preserved at %s (inspect: duckdb -readonly %q)", dbPath, dbPath)
	})
	return a
}

// loadTestEnv prefers tests/scenarios/.test.env; falls back to the
// repo-root .env (the default testenv.LoadDotEnv behaviour). Both are
// optional — Setup's EnvOrSkip checks decide whether to run.
func loadTestEnv(t *testing.T) {
	t.Helper()
	testEnv := filepath.Join(scenariosRoot(), ".test.env")
	if data, err := os.ReadFile(testEnv); err == nil {
		applyEnvFile(string(data))
		return
	}
	testenv.LoadDotEnv()
}

// applyEnvFile mirrors internal/testenv.applyEnvFile but is inlined
// here to avoid exporting it just for scenarios. Already-set vars
// win over file-provided values so shell exports always take priority.
func applyEnvFile(body string) {
	for _, raw := range splitLines(body) {
		line := trimSpace(raw)
		if line == "" || line[0] == '#' {
			continue
		}
		eq := indexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := trimSpace(line[:eq])
		val := trimSpace(line[eq+1:])
		if len(val) >= 2 {
			first, last := val[0], val[len(val)-1]
			if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		if _, set := os.LookupEnv(key); set {
			continue
		}
		_ = os.Setenv(key, val)
	}
}

// --- tiny string helpers (avoid strings import bloat) ---
func splitLines(s string) []string {
	out := []string{}
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// configOverride is the subset of *config.Config a scenario may
// override via <scenarioDir>/config_override.yaml.
type configOverride struct {
	LLM *struct {
		Model          *string           `yaml:"model"`
		ContextWindows map[string]int    `yaml:"context_windows"`
		DefaultBudget  *int              `yaml:"default_budget"`
		Routes         map[string]string `yaml:"routes"`
	} `yaml:"llm"`
	ChatContext *struct {
		CompactionThreshold *float64 `yaml:"compaction_threshold"`
	} `yaml:"chatcontext"`
}

func applyConfigOverride(cfg *config.Config, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("harness: read config override %s: %w", path, err)
	}
	var ov configOverride
	if err := yaml.Unmarshal(data, &ov); err != nil {
		return fmt.Errorf("harness: parse config override %s: %w", path, err)
	}
	if ov.LLM != nil {
		if ov.LLM.Model != nil {
			cfg.LLM.Model = *ov.LLM.Model
		}
		if ov.LLM.DefaultBudget != nil {
			cfg.LLM.DefaultBudget = *ov.LLM.DefaultBudget
		}
		if cfg.LLM.ContextWindows == nil {
			cfg.LLM.ContextWindows = map[string]int{}
		}
		for k, v := range ov.LLM.ContextWindows {
			cfg.LLM.ContextWindows[k] = v
		}
		if cfg.LLM.Routes == nil {
			cfg.LLM.Routes = map[string]string{}
		}
		for k, v := range ov.LLM.Routes {
			cfg.LLM.Routes[k] = v
		}
	}
	if ov.ChatContext != nil && ov.ChatContext.CompactionThreshold != nil {
		cfg.ChatContext.CompactionThreshold = *ov.ChatContext.CompactionThreshold
	}
	return nil
}

// --- path helpers ---

func scenarioDataDir() string { return filepath.Join(scenariosRoot(), ".data") }

func scenariosRoot() string {
	if _, file, _, ok := runtime.Caller(0); ok {
		return filepath.Dir(filepath.Dir(file)) // harness/ → scenarios/
	}
	return "tests/scenarios"
}

func fixtureSkillsPath() string {
	if _, file, _, ok := runtime.Caller(0); ok {
		return filepath.Join(filepath.Dir(file), "skills")
	}
	return "tests/scenarios/harness/skills"
}

// buildSkillsDir creates a tmp skills dir seeded with the production
// `_system` / `_memory` / `_context` skills + every subdir under the
// fixture path — keeps the fixture isolated from the live `skills/`
// tree while still running on real system skills.
func buildSkillsDir(t *testing.T, fixtureDir string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "skills")
	require.NoError(t, os.MkdirAll(dst, 0o755))

	root := repoRoot()
	for _, name := range []string{"_system", "_memory", "_context"} {
		src := filepath.Join(root, "skills", name)
		require.NoError(t, copyDir(src, filepath.Join(dst, name)),
			"harness: copy system skill %s", name)
	}
	entries, err := os.ReadDir(fixtureDir)
	require.NoError(t, err)
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		require.NoError(t, copyDir(
			filepath.Join(fixtureDir, ent.Name()),
			filepath.Join(dst, ent.Name()),
		))
	}
	return dst
}

func repoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("harness: cannot determine source file for repo root")
	}
	dir := filepath.Dir(file)
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	panic("harness: go.mod not found")
}

func copyDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, ent := range entries {
		sp := filepath.Join(src, ent.Name())
		dp := filepath.Join(dst, ent.Name())
		if ent.IsDir() {
			if err := copyDir(sp, dp); err != nil {
				return err
			}
			continue
		}
		data, err := os.ReadFile(sp)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dp, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}
