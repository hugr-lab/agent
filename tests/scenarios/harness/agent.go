//go:build duckdb_arrow && scenario

// Package harness spins up a full hugr-agent runtime for scenario
// runners under tests/scenarios/. Uses pkg/runtime.Build directly so
// the wiring matches production exactly — no drift between cmd/agent
// and the harness.
//
// A scenario is a directory containing scenario.yaml (+ optional
// config_override.yaml). The runner drives user messages + GraphQL
// queries defined in the YAML and leaves hub.db on disk for manual
// DuckDB inspection. Nothing here is a strict assertion; LLM output
// is non-deterministic, so scenarios are observational.
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
	"github.com/hugr-lab/hugen/pkg/chatcontext"
	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/models"
	hugenruntime "github.com/hugr-lab/hugen/pkg/runtime"
	"github.com/hugr-lab/hugen/pkg/skills"
	"github.com/hugr-lab/hugen/pkg/store/local"
	"github.com/hugr-lab/hugen/pkg/tools"

	qetypes "github.com/hugr-lab/query-engine/types"
)

// Opts tunes Setup. ScenarioName is the only required field.
type Opts struct {
	// ScenarioName names the hub.db artefact + the harness session id
	// prefix. Required.
	ScenarioName string

	// ConfigOverridePath, when non-empty, is a yaml file whose contents
	// are overlaid onto the baseline scenario config (see
	// applyConfigOverride). Typically <scenarioDir>/config_override.yaml.
	ConfigOverridePath string

	// PersistDB, when non-empty, overrides the hub.db artefact path.
	// Equivalent to env SCENARIO_PERSIST. Empty → tests/scenarios/.data/
	// <name>-<timestamp>/memory.db.
	PersistDB string
}

// Agent bundles everything scenarios need.
type Agent struct {
	Runtime *hugenruntime.Runtime // Agent / Sessions / Engine / Tools / Skills
	AgentID string
	AppName string
	UserID  string
	HubPath string

	logger *slog.Logger
}

// Engine is a shortcut for callers that just want the Querier.
func (a *Agent) Engine() qetypes.Querier { return a.Runtime.Querier }

// Close delegates to Runtime.Close. Already registered as t.Cleanup by
// Setup; exposed so scenarios can call it early if needed.
func (a *Agent) Close() { a.Runtime.Close() }

// Setup builds the runtime and wires t.Cleanup. Skips when required
// env vars are missing.
func Setup(t *testing.T, o Opts) *Agent {
	t.Helper()
	require.NotEmpty(t, o.ScenarioName, "harness.Setup: ScenarioName required")

	llmURL := testenv.EnvOrSkip(t, "LLM_LOCAL_URL")
	embedURL := testenv.EnvOrSkip(t, "EMBED_LOCAL_URL")

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

	// --- Baseline scenario config ---
	cfg := baselineConfig(skillsPath, dbPath, coreDBDir, llmURL, embedURL)

	// --- Apply per-scenario override ---
	if o.ConfigOverridePath != "" {
		require.NoError(t, applyConfigOverride(cfg, o.ConfigOverridePath))
	}

	// --- Build ---
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

// baselineConfig builds the minimal *config.Config used by every
// scenario. Fields not overridden by a scenario's config_override.yaml
// stay at these values.
func baselineConfig(skillsPath, dbPath, coreDBDir, llmURL, embedURL string) *config.Config {
	cfg := &config.Config{}
	cfg.Identity = local.Identity{
		ID:      "agt_ag01",
		ShortID: "ag01",
		Name:    "scenario-agent",
		Type:    "hugr-data",
	}
	cfg.Embedding = local.EmbeddingConfig{
		Mode:      "local",
		Model:     "gemma-embedding",
		Dimension: 768,
	}
	cfg.LocalDBEnabled = true
	cfg.LocalDB = local.Config{
		DB: local.DBConfig{
			Path: filepath.Join(coreDBDir, "engine.db"),
			Settings: local.DBSettings{
				MaxMemory:     4,
				WorkerThreads: 2,
				HomeDirectory: coreDBDir,
				Timezone:      "UTC",
			},
		},
		MemoryPath: dbPath,
		Models: []local.ModelDef{
			{
				Name: "gemma4-26b",
				Type: "llm-openai",
				Path: llmURL + `?model="google/gemma-4-26b-a4b"&max_tokens=8096&thinking_budget=2048&timeout=10m`,
			},
			{
				Name: "gemma-small",
				Type: "llm-openai",
				Path: llmURL + `?model="google/gemma-4-e2b-it"&timeout=120s`,
			},
			{
				Name: "gemma-embedding",
				Type: "embedding",
				Path: embedURL + `?model="text-embedding-embeddinggemma-300m-qat"&timeout=30s`,
			},
		},
	}
	cfg.LLM = models.Config{
		Model: "gemma4-26b",
		Routes: map[string]string{
			"default":      "gemma4-26b",
			"tool_calling": "gemma4-26b",
		},
		ContextWindows: map[string]int{
			"gemma4-26b":  50_000,
			"gemma-small": 32_000,
		},
		DefaultBudget: 64_000,
		MaxTokens:     8096,
		Temperature:   0.4,
	}
	cfg.ChatContext = chatcontext.Config{CompactionThreshold: 0.7}
	cfg.Skills = skills.Config{Path: skillsPath}
	cfg.Agent.Constitution = filepath.Join(repoRoot(), "constitution", "base.md")
	cfg.MCP = tools.MCPConfig{TTL: 60 * time.Second, FetchTimeout: 30 * time.Second}
	return cfg
}

// configOverride is the subset of *config.Config fields a scenario is
// allowed to override via config_override.yaml. Restricted on purpose
// — the scenario shouldn't be re-declaring identity / paths / skills.
type configOverride struct {
	LLM *struct {
		Model          *string         `yaml:"model"`
		ContextWindows map[string]int  `yaml:"context_windows"`
		DefaultBudget  *int            `yaml:"default_budget"`
		Routes         map[string]string `yaml:"routes"`
	} `yaml:"llm"`
	ChatContext *struct {
		CompactionThreshold *float64 `yaml:"compaction_threshold"`
	} `yaml:"chatcontext"`
}

// applyConfigOverride overlays a scenario-level config_override.yaml
// onto the already-built cfg.
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
		for k, v := range ov.LLM.ContextWindows {
			if cfg.LLM.ContextWindows == nil {
				cfg.LLM.ContextWindows = map[string]int{}
			}
			cfg.LLM.ContextWindows[k] = v
		}
		for k, v := range ov.LLM.Routes {
			if cfg.LLM.Routes == nil {
				cfg.LLM.Routes = map[string]string{}
			}
			cfg.LLM.Routes[k] = v
		}
	}
	if ov.ChatContext != nil && ov.ChatContext.CompactionThreshold != nil {
		cfg.ChatContext.CompactionThreshold = *ov.ChatContext.CompactionThreshold
	}
	return nil
}

// --- path helpers ---

func scenarioDataDir() string {
	if _, file, _, ok := runtime.Caller(0); ok {
		return filepath.Join(filepath.Dir(filepath.Dir(file)), ".data")
	}
	return ".data/scenarios"
}

func fixtureSkillsPath() string {
	if _, file, _, ok := runtime.Caller(0); ok {
		return filepath.Join(filepath.Dir(file), "skills")
	}
	return "tests/scenarios/harness/skills"
}

// buildSkillsDir creates a tmp skills dir seeded with the production
// `_system` / `_memory` / `_context` skills + every subdir under the
// fixture path. Keeps the fixture isolated from the live `skills/`
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
