//go:build duckdb_arrow && scenario

// Package scenarios walks tests/scenarios/*/scenario.yaml and plays
// each scenario end-to-end against a fresh local-agent runtime. One
// sub-test per scenario (via t.Run) so `go test -run TestScenarios/simple`
// narrows to a single run.
//
// Every scenario is observational — no hard assertions on the
// non-deterministic LLM output. The scenario's GraphQL queries (one
// per step, with optional jq shaping) are logged into t.Log for
// manual review and the hub.db file is preserved on disk at
// tests/scenarios/.data/<name>-<ts>/ for follow-up DuckDB inspection.
package scenarios_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/hugr-lab/hugen/tests/scenarios/harness"
)

// scenario is the on-disk yaml shape.
type scenario struct {
	Name      string        `yaml:"name"`
	SessionID string        `yaml:"session_id"`
	Steps     []scenarioStep `yaml:"steps"`
	// ConfigOverride is a sibling file whose path is resolved relative
	// to the scenario directory. Optional.
	ConfigOverride string `yaml:"config_override"`
}

type scenarioStep struct {
	Say     string          `yaml:"say"`
	Queries []scenarioQuery `yaml:"queries"`
}

type scenarioQuery struct {
	Name    string         `yaml:"name"`
	GraphQL string         `yaml:"graphql"`
	Vars    map[string]any `yaml:"vars"`
	// Path, when set, is passed to resp.ScanDataJSON(path, ...) and the
	// pretty-printed value is logged. Empty path logs resp.Data as-is.
	Path string `yaml:"path"`
}

// TestScenarios walks every `<name>/scenario.yaml` under this package
// and runs a sub-test per file. Env filter via SCENARIO_NAME (set by
// `make scenario name=X`).
func TestScenarios(t *testing.T) {
	root := scenariosRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read scenarios root %s: %v", root, err)
	}

	filter := os.Getenv("SCENARIO_NAME")
	var ran int
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		if ent.Name() == "harness" || strings.HasPrefix(ent.Name(), ".") {
			continue
		}
		scenPath := filepath.Join(root, ent.Name(), "scenario.yaml")
		if _, err := os.Stat(scenPath); err != nil {
			continue // skip dirs without a scenario.yaml
		}
		if filter != "" && ent.Name() != filter {
			continue
		}
		ran++
		t.Run(ent.Name(), func(t *testing.T) {
			runScenario(t, filepath.Dir(scenPath), scenPath)
		})
	}
	if ran == 0 && filter != "" {
		t.Fatalf("no scenario matched SCENARIO_NAME=%q under %s", filter, root)
	}
}

func runScenario(t *testing.T, scenDir, scenPath string) {
	t.Helper()

	raw, err := os.ReadFile(scenPath)
	if err != nil {
		t.Fatalf("read %s: %v", scenPath, err)
	}
	var sc scenario
	if err := yaml.Unmarshal(raw, &sc); err != nil {
		t.Fatalf("parse %s: %v", scenPath, err)
	}
	if sc.Name == "" {
		sc.Name = filepath.Base(scenDir)
	}
	if sc.SessionID == "" {
		sc.SessionID = "scenario-" + sc.Name + "-1"
	}

	opts := harness.Opts{ScenarioName: sc.Name}
	if sc.ConfigOverride != "" {
		opts.ConfigOverridePath = filepath.Join(scenDir, sc.ConfigOverride)
	}

	a := harness.Setup(t, opts)
	ctx := context.Background()

	if err := a.CreateSession(ctx, sc.SessionID); err != nil {
		t.Fatalf("create session %s: %v", sc.SessionID, err)
	}

	for i, step := range sc.Steps {
		t.Logf("════ step %d/%d ════", i+1, len(sc.Steps))
		if step.Say != "" {
			a.RunTurn(ctx, t, sc.SessionID, step.Say)
		}
		// Drain the async classifier so subsequent queries + the Dump
		// see every llm_response / tool_* event the turn emitted.
		drainClassifier(t, a, 5*time.Second)
		for _, q := range step.Queries {
			runQuery(ctx, t, a, q)
		}
	}

	// One more drain before the final snapshot — compactor /
	// subagent-dispatch flows publish events after the coordinator's
	// turn completes.
	drainClassifier(t, a, 5*time.Second)
	t.Logf("════ final hub.db snapshot ════")
	a.Inspect().Dump(ctx, t, sc.SessionID)
}

// drainClassifier flushes the async transcript classifier + gives the
// hub a short breather so GraphQL reads pick up every event the just-
// completed turn produced.
func drainClassifier(t *testing.T, a *harness.Agent, budget time.Duration) {
	t.Helper()
	if a.Runtime == nil || a.Runtime.Classifier == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	if err := a.Runtime.Classifier.Drain(ctx, budget); err != nil {
		t.Logf("classifier drain: %v", err)
	}
}

// runQuery fires one scenario query against the live engine and logs
// the response. Never fails the test — scenarios are observational.
func runQuery(ctx context.Context, t *testing.T, a *harness.Agent, q scenarioQuery) {
	t.Helper()
	if q.GraphQL == "" {
		return
	}
	label := q.Name
	if label == "" {
		label = "(unnamed)"
	}
	resp, err := a.Engine().Query(ctx, q.GraphQL, q.Vars)
	if err != nil {
		t.Logf("── query %s ── transport error: %v", label, err)
		return
	}
	if resp != nil {
		defer resp.Close()
	}
	if resp == nil {
		t.Logf("── query %s ── nil response", label)
		return
	}
	if ge := resp.Err(); ge != nil {
		t.Logf("── query %s ── graphql errors: %v", label, ge)
	}

	var payload any
	if q.Path != "" {
		var shaped any
		if err := resp.ScanDataJSON(q.Path, &shaped); err != nil {
			t.Logf("── query %s ── scan path %q: %v", label, q.Path, err)
			return
		}
		payload = shaped
	} else {
		payload = map[string]any{"data": resp.Data, "extensions": resp.Extensions}
	}

	pretty, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Logf("── query %s ── marshal: %v", label, err)
		return
	}
	t.Logf("── query %s ──\n%s", label, truncate(string(pretty), 4096))
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n… (truncated)"
}

func scenariosRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(file)
}

// used by some editors / interface checks — keeps the import live.
var _ = fmt.Sprintf
