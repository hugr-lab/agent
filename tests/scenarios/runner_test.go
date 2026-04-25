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

	"github.com/hugr-lab/hugen/pkg/missions/graph"
	"github.com/hugr-lab/hugen/tests/scenarios/harness"
)

// scenario is the on-disk yaml shape.
type scenario struct {
	Name      string         `yaml:"name"`
	SessionID string         `yaml:"session_id"`
	Steps     []scenarioStep `yaml:"steps"`
	// ConfigOverride is a sibling file whose path is resolved relative
	// to the scenario directory. Optional.
	ConfigOverride string `yaml:"config_override"`
	// FinalWait, when set, polls the coordinator's event count and
	// only continues to the hub.db dump once it stabilises (no new
	// events in 5 s) or the budget expires. Use for scenarios that
	// expect spec-007 RunOnce auto-fire to land an llm_response on
	// the coord after the test's last user-driven step. Accepts any
	// time.ParseDuration value ("60s", "2m"). Default: skip.
	FinalWait string `yaml:"final_wait"`
}

type scenarioStep struct {
	Say     string          `yaml:"say"`
	Queries []scenarioQuery `yaml:"queries"`
	// WaitForMissions, when non-empty, polls the Executor's DAG for
	// this coordinator after the step's turn completes and blocks
	// until every mission reaches a terminal status — or the budget
	// expires. Accepts any time.ParseDuration value ("30s", "2m").
	WaitForMissions string `yaml:"wait_for_missions"`
	// WaitForMissionsRunning polls until at least one mission in the
	// coordinator's DAG is in StatusRunning (and thus safe to send a
	// refinement to via follow-up routing). Use before a refinement
	// step so the router has an in-flight target to classify against.
	WaitForMissionsRunning string `yaml:"wait_for_missions_running"`
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
		// Pre-conditions: wait for some DAG state BEFORE sending the
		// next user message. Typical use case — follow-up routing
		// refinement needs the targeted mission already running.
		if step.WaitForMissionsRunning != "" {
			waitMissionsRunning(ctx, t, a, sc.SessionID, step.WaitForMissionsRunning)
			drainClassifier(t, a, 5*time.Second)
		}
		if step.Say != "" {
			a.RunTurn(ctx, t, sc.SessionID, step.Say)
		}
		// Drain the async classifier so subsequent queries + the Dump
		// see every llm_response / tool_* event the turn emitted.
		drainClassifier(t, a, 5*time.Second)
		// Post-conditions: wait for the DAG to terminate before we
		// sample the final evidence.
		if step.WaitForMissions != "" {
			waitMissionsTerminal(ctx, t, a, sc.SessionID, step.WaitForMissions)
			drainClassifier(t, a, 5*time.Second)
		}
		for _, q := range step.Queries {
			runQuery(ctx, t, a, q)
		}
	}

	// One more drain before the final snapshot. Extended budget so
	// async post-turn work has time to land — compactor + subagent
	// dispatch + spec-007 RunOnce auto-fire (which kicks off a fresh
	// runner.Run on a detached ctx after the DAG terminates).
	drainClassifier(t, a, 30*time.Second)
	if sc.FinalWait != "" {
		waitForCoordIdle(ctx, t, a, sc.SessionID, sc.FinalWait)
		drainClassifier(t, a, 5*time.Second)
	}
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
	if err := a.Runtime.Classifier.Flush(ctx, budget); err != nil {
		t.Logf("classifier flush: %v", err)
	}
}

// waitMissionsTerminal polls the Executor's in-memory DAG for
// coordSessionID until every mission reaches a terminal status or the
// budget expires. Logs periodic progress at 5s intervals. Never
// fails the test — scenarios are observational, a timeout simply
// surfaces in the log for inspection.
func waitMissionsTerminal(
	ctx context.Context,
	t *testing.T,
	a *harness.Agent,
	coordSessionID string,
	budget string,
) {
	t.Helper()
	if a.Runtime == nil || a.Runtime.Missions == nil {
		return
	}
	d, err := time.ParseDuration(budget)
	if err != nil {
		t.Logf("wait_for_missions: parse %q: %v", budget, err)
		return
	}
	deadline := time.Now().Add(d)
	lastLog := time.Now()
	for {
		nodes := a.Runtime.Missions.Snapshot(ctx, coordSessionID)
		if missionsAllTerminal(nodes) {
			t.Logf("wait_for_missions: %d missions terminal after %s",
				len(nodes), time.Since(deadline.Add(-d)).Truncate(time.Millisecond))
			return
		}
		if time.Now().After(deadline) {
			t.Logf("wait_for_missions: timeout after %s — %d missions still non-terminal",
				d, countNonTerminal(nodes))
			return
		}
		if time.Since(lastLog) > 5*time.Second {
			lastLog = time.Now()
			t.Logf("wait_for_missions: %d missions still running / pending",
				countNonTerminal(nodes))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// waitForCoordIdle polls the coordinator's session_events for a
// NEW llm_response after the count we observed at entry. Returns as
// soon as such an event lands or the budget expires. Used as the
// final settle gate for scenarios whose last meaningful event lands
// asynchronously after the test's user-driven steps — spec-007's
// RunOnce auto-fire being the canonical example.
func waitForCoordIdle(
	ctx context.Context,
	t *testing.T,
	a *harness.Agent,
	coordSessionID string,
	budget string,
) {
	t.Helper()
	d, err := time.ParseDuration(budget)
	if err != nil {
		t.Logf("final_wait: parse %q: %v", budget, err)
		return
	}
	in := a.Inspect()
	startCount := in.EventCount(ctx, coordSessionID)
	if startCount < 0 {
		t.Logf("final_wait: event count failed at entry")
		return
	}
	deadline := time.Now().Add(d)
	for {
		// Look at the latest events; bail as soon as a fresh
		// llm_response lands past the entry watermark.
		evs := in.LatestEvents(ctx, coordSessionID, startCount+8)
		for _, ev := range evs {
			if ev.Seq <= startCount {
				continue
			}
			if ev.EventType == "llm_response" {
				t.Logf("final_wait: coord auto-fire produced llm_response at seq %d after %s",
					ev.Seq, time.Since(deadline.Add(-d)).Truncate(time.Millisecond))
				return
			}
		}
		if time.Now().After(deadline) {
			t.Logf("final_wait: timeout after %s — no auto-fire llm_response on coord", d)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// waitMissionsRunning polls the Executor's DAG until at least one
// mission is in StatusRunning — sentinel used before a refinement
// step so the follow-up router has an in-flight target. Exits early
// when the budget expires (scenario stays observational — no fatal).
func waitMissionsRunning(
	ctx context.Context,
	t *testing.T,
	a *harness.Agent,
	coordSessionID string,
	budget string,
) {
	t.Helper()
	if a.Runtime == nil || a.Runtime.Missions == nil {
		return
	}
	d, err := time.ParseDuration(budget)
	if err != nil {
		t.Logf("wait_for_missions_running: parse %q: %v", budget, err)
		return
	}
	deadline := time.Now().Add(d)
	for {
		running := a.Runtime.Missions.RunningMissions(coordSessionID)
		if len(running) > 0 {
			t.Logf("wait_for_missions_running: %d running after %s",
				len(running), time.Since(deadline.Add(-d)).Truncate(time.Millisecond))
			return
		}
		if time.Now().After(deadline) {
			t.Logf("wait_for_missions_running: timeout after %s — no mission reached running", d)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func missionsAllTerminal(nodes []graph.MissionRecord) bool {
	if len(nodes) == 0 {
		return true
	}
	for _, n := range nodes {
		switch n.Status {
		case graph.StatusDone, graph.StatusFailed, graph.StatusAbandoned:
			continue
		default:
			return false
		}
	}
	return true
}

func countNonTerminal(nodes []graph.MissionRecord) int {
	n := 0
	for _, m := range nodes {
		switch m.Status {
		case graph.StatusDone, graph.StatusFailed, graph.StatusAbandoned:
			continue
		default:
			n++
		}
	}
	return n
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
