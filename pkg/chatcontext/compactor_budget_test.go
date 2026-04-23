package chatcontext

import (
	"bytes"
	"context"
	"iter"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/hugr-lab/hugen/pkg/models"
)

// quietLLM is a no-op LLM used as a router default so BudgetFor can
// resolve a model name without dragging in the hugr adapter.
type quietLLM struct{ name string }

func (q *quietLLM) Name() string { return q.name }
func (q *quietLLM) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{TurnComplete: true}, nil)
	}
}

// budgetCompactor builds a Compactor wired only to what usageRatio
// reads — router + tokens. Bypasses NewCompactor (which requires a
// real Querier for the hub stores).
func budgetCompactor(intent models.Intent, modelName string, budget int, defaultBudget int) (*Compactor, *bytes.Buffer) {
	router := models.NewRouterWithDefault(&quietLLM{name: modelName})
	logBuf := &bytes.Buffer{}
	router.WithLogger(slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if budget > 0 {
		router.SetBudgets(map[string]int{modelName: budget}, defaultBudget)
	} else {
		router.SetBudgets(nil, defaultBudget)
	}
	return &Compactor{
		router: router,
		tokens: models.NewTokenEstimator(),
		intent: intent,
	}, logBuf
}

// reqOfSize returns an LLMRequest whose Contents text totals
// approximately `chars` bytes — enough to drive usageRatio
// deterministically. Single user message, no FunctionResponse parts
// (those would add the +2000 hack).
func reqOfSize(chars int) *model.LLMRequest {
	return &model.LLMRequest{
		Contents: []*genai.Content{
			{
				Role:  "user",
				Parts: []*genai.Part{{Text: strings.Repeat("x", chars)}},
			},
		},
	}
}

// TestUsageRatio_FollowsRouterBudget — same prompt size, two
// different model windows ⇒ very different ratios.
func TestUsageRatio_FollowsRouterBudget(t *testing.T) {
	// Estimator default ≈ 4 chars/token, so 200 000 chars ≈ 50 000
	// tokens. Against a 50 000-token cheap model that's already at
	// 1.0; against a 1 000 000-token strong model it's 0.05.
	const chars = 200_000

	cheap, _ := budgetCompactor(models.IntentDefault, "cheap-50k", 50_000, 0)
	strong, _ := budgetCompactor(models.IntentDefault, "strong-1m", 1_000_000, 0)

	cheapRatio := cheap.usageRatio(reqOfSize(chars))
	strongRatio := strong.usageRatio(reqOfSize(chars))

	assert.InDelta(t, 1.0, cheapRatio, 0.05,
		"cheap-50k @ 200k chars should sit near 100%% (got %.3f)", cheapRatio)
	assert.InDelta(t, 0.05, strongRatio, 0.005,
		"strong-1m @ 200k chars should sit near 5%% (got %.3f)", strongRatio)
	// Sanity: ratio scales linearly with budget.
	assert.Greater(t, cheapRatio, strongRatio*15,
		"cheap ratio must be at least 15× the strong ratio at the same prompt")
}

// TestUsageRatio_ThresholdFiringPoint — verifies that a 0.75
// threshold trips at ~75% of the model's window. Not a literal
// trigger of the compactor (which needs hub clients) but a direct
// usageRatio comparison.
func TestUsageRatio_ThresholdFiringPoint(t *testing.T) {
	const threshold = 0.75
	const cheapBudget = 50_000

	cheap, _ := budgetCompactor(models.IntentDefault, "cheap-50k", cheapBudget, 0)

	// At 0.75 × 50 000 tokens × ~4 chars/token = 150 000 chars we
	// should cross. Nudge above + below.
	below := cheap.usageRatio(reqOfSize(140_000))
	above := cheap.usageRatio(reqOfSize(160_000))

	assert.Less(t, below, threshold,
		"140k chars on cheap-50k should be below 0.75 (got %.3f)", below)
	assert.Greater(t, above, threshold,
		"160k chars on cheap-50k should be above 0.75 (got %.3f)", above)
}

// TestUsageRatio_FallsBackToDefaultBudget — when ContextWindows
// doesn't cover the routed model, BudgetFor returns DefaultBudget,
// and usageRatio uses it. No INFO log because DefaultBudget covers.
func TestUsageRatio_FallsBackToDefaultBudget(t *testing.T) {
	c, logBuf := budgetCompactor(models.IntentDefault, "unknown-model", 0, 200_000)

	ratio := c.usageRatio(reqOfSize(200_000))
	// 200 000 chars / ~4 = 50 000 tokens / 200 000 budget = 0.25.
	assert.InDelta(t, 0.25, ratio, 0.02,
		"DefaultBudget=200k @ 200k chars should sit at ~25%% (got %.3f)", ratio)
	assert.Empty(t, logBuf.String(),
		"DefaultBudget hits should not produce a router INFO line")
}

// TestUsageRatio_FloorWhenNoBudget — neither ContextWindows nor
// DefaultBudget configured ⇒ 128 000 floor with one INFO log per
// intent, asserted by spec 006 plan.
func TestUsageRatio_FloorWhenNoBudget(t *testing.T) {
	c, logBuf := budgetCompactor(models.IntentDefault, "unknown-model", 0, 0)

	// 128 000 chars / 4 = 32 000 tokens / 128 000 budget = 0.25
	ratio := c.usageRatio(reqOfSize(128_000))
	assert.InDelta(t, 0.25, ratio, 0.02,
		"floor 128k @ 128k chars should sit at ~25%% (got %.3f)", ratio)
	logged := logBuf.String()
	assert.NotEmpty(t, logged,
		"floor fallback must emit one INFO log line")
	assert.Contains(t, logged, `"intent":"default"`)
	assert.Contains(t, logged, `"floor":128000`)

	// Repeated calls don't add new lines (one-shot per intent).
	prev := logBuf.Len()
	for i := 0; i < 5; i++ {
		_ = c.usageRatio(reqOfSize(1024))
	}
	assert.Equal(t, prev, logBuf.Len(),
		"floor fallback INFO must fire at most once per intent")
}

// TestUsageRatio_DefensiveFloorOnZeroRouter — guard branch: if
// BudgetFor were ever to return 0 (mis-built router), usageRatio
// keeps the historical floor. Defensive belt-and-braces.
func TestUsageRatio_DefensiveFloorOnZeroRouter(t *testing.T) {
	// Build a Compactor whose router has no default model AND no
	// budgets — BudgetFor returns the floor (128k) which is what the
	// usageRatio fallback would also pick. We confirm ratios match
	// the floor rather than going to +Inf.
	c, _ := budgetCompactor(models.IntentDefault, "", 0, 0)
	ratio := c.usageRatio(reqOfSize(64_000))
	// 64k chars / 4 = 16k tokens / 128k floor = 0.125
	assert.InDelta(t, 0.125, ratio, 0.02,
		"empty-default + empty-budget must fall back to 128k floor (got %.3f)", ratio)
}
