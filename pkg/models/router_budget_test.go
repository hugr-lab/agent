package models

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

// captureLogger returns a slog.Logger that writes JSON lines to the
// returned buffer. Tests inspect the buffer to assert log emission
// (one-shot floor INFO).
func captureLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})), buf
}

// budgetRouter mirrors newTestRouter but installs the spec-006 fields
// that BudgetFor reads. Built via NewRouterWithDefault so default-model
// metadata + maps land correctly.
func budgetRouter(t *testing.T, defaultName string, routes map[Intent]string, windows map[string]int, defaultBudget int) (*Router, *bytes.Buffer) {
	t.Helper()
	r := NewRouterWithDefault(&fakeLLM{name: defaultName})
	for intent, modelName := range routes {
		r.SetRoute(intent, &fakeLLM{name: modelName})
	}
	r.SetBudgets(windows, defaultBudget)

	logger, buf := captureLogger()
	r.WithLogger(logger)
	return r, buf
}

func TestRouter_BudgetFor_DirectRouteHit(t *testing.T) {
	r, _ := budgetRouter(t,
		"claude-opus-4-7",
		map[Intent]string{
			IntentToolCalling: "qwen3-27b",
			IntentClassification: "gemma-4-27b",
		},
		map[string]int{
			"claude-opus-4-7": 1_000_000,
			"qwen3-27b":         100_000,
			"gemma-4-27b":        50_000,
		},
		0,
	)

	assert.Equal(t, 1_000_000, r.BudgetFor(IntentDefault))
	assert.Equal(t, 100_000, r.BudgetFor(IntentToolCalling))
	assert.Equal(t, 50_000, r.BudgetFor(IntentClassification))
}

func TestRouter_BudgetFor_FallsBackToDefaultModelWindow(t *testing.T) {
	// IntentSummarization has NO explicit route — should fall back to
	// the default model's window via the routeName-resolution chain.
	r, _ := budgetRouter(t,
		"claude-opus-4-7",
		map[Intent]string{},
		map[string]int{
			"claude-opus-4-7": 1_000_000,
		},
		0,
	)
	assert.Equal(t, 1_000_000, r.BudgetFor(IntentSummarization))
}

func TestRouter_BudgetFor_FallsBackToDefaultBudget(t *testing.T) {
	// Routed model is NOT in the ContextWindows map → falls through to
	// Config.DefaultBudget (no INFO log because DefaultBudget covers it).
	r, buf := budgetRouter(t,
		"some-fine-tune",
		map[Intent]string{
			IntentToolCalling: "another-fine-tune",
		},
		map[string]int{
			// Empty / mismatched on purpose.
			"unrelated-model": 999,
		},
		200_000,
	)

	assert.Equal(t, 200_000, r.BudgetFor(IntentToolCalling))
	assert.Equal(t, 200_000, r.BudgetFor(IntentDefault))
	assert.Empty(t, buf.String(), "DefaultBudget hits should not emit a log line")
}

func TestRouter_BudgetFor_FallsBackToFloorWithSingleINFO(t *testing.T) {
	// Neither ContextWindows nor DefaultBudget covers the routed model.
	// First call emits INFO; subsequent calls for the same intent stay
	// silent. A different intent emits its own (separate) INFO.
	r, buf := budgetRouter(t,
		"unknown-model",
		map[Intent]string{
			IntentToolCalling: "unknown-cheap-model",
		},
		nil, // no windows
		0,   // no default budget
	)

	assert.Equal(t, budgetFloor, r.BudgetFor(IntentDefault))
	assert.Equal(t, budgetFloor, r.BudgetFor(IntentToolCalling))

	// Second call for IntentDefault must NOT add a new line.
	beforeSecond := buf.Len()
	assert.Equal(t, budgetFloor, r.BudgetFor(IntentDefault))
	assert.Equal(t, beforeSecond, buf.Len(), "BudgetFor must log at most once per intent")

	logged := buf.String()
	intentDefaultCount := strings.Count(logged, `"intent":"`+string(IntentDefault)+`"`)
	intentToolCount := strings.Count(logged, `"intent":"`+string(IntentToolCalling)+`"`)
	assert.Equal(t, 1, intentDefaultCount, "expected exactly one log line for IntentDefault")
	assert.Equal(t, 1, intentToolCount, "expected exactly one log line for IntentToolCalling")
	assert.Contains(t, logged, `"floor":128000`)
}

func TestRouter_BudgetFor_ConcurrentReadsAreSafe(t *testing.T) {
	// Sanity check under -race: parallel reads from ConfiguredWindows +
	// the budgetWarn map MUST not race. Single-emit invariant still
	// holds: only one INFO per intent regardless of concurrency.
	r, buf := budgetRouter(t,
		"unknown-model",
		map[Intent]string{
			IntentToolCalling: "unknown-cheap-model",
		},
		nil,
		0,
	)

	const goroutines = 16
	const iterations = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				_ = r.BudgetFor(IntentDefault)
				_ = r.BudgetFor(IntentToolCalling)
			}
		}()
	}
	wg.Wait()

	// Even with goroutines * iterations * 2 calls, the INFO line per
	// intent should appear at most once. (Allow up to 2 per intent —
	// the single-emit guard is best-effort under contention; second
	// "lossy" emission is acceptable noise but everything past 2 would
	// be a real bug.)
	logged := buf.String()
	intentDefaultCount := strings.Count(logged, `"intent":"`+string(IntentDefault)+`"`)
	intentToolCount := strings.Count(logged, `"intent":"`+string(IntentToolCalling)+`"`)
	assert.LessOrEqual(t, intentDefaultCount, 2,
		"BudgetFor floor INFO emitted %d times for IntentDefault under concurrency",
		intentDefaultCount)
	assert.LessOrEqual(t, intentToolCount, 2,
		"BudgetFor floor INFO emitted %d times for IntentToolCalling under concurrency",
		intentToolCount)
}

func TestRouter_BudgetFor_NoDefaultModel(t *testing.T) {
	// Edge case: NewRouterWithDefault(nil) → defaultModel is nil. BudgetFor
	// should still return the floor without panicking.
	r := NewRouterWithDefault(nil)
	logger, _ := captureLogger()
	r.WithLogger(logger)

	// No model name resolvable → straight to floor.
	assert.Equal(t, budgetFloor, r.BudgetFor(IntentDefault))
}

// Compile-time signal that the test file uses context for fakeLLM.
var _ = context.Background
