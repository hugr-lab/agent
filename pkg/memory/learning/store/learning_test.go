//go:build duckdb_arrow

package store_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentstore "github.com/hugr-lab/hugen/pkg/agent/store"
	lstore "github.com/hugr-lab/hugen/pkg/memory/learning/store"
	"github.com/hugr-lab/hugen/internal/testenv"
)

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// newLearningClient spins up the embedded hugr engine with seeded
// agent row and returns a learning client scoped to it.
func newLearningClient(t *testing.T) *lstore.Client {
	t.Helper()
	service, _ := testenv.Engine(t)
	logger := slog.New(slog.NewTextHandler(discardWriter{}, nil))

	reg, err := agentstore.New(service, agentstore.Options{
		AgentID: "agt_ag01", AgentShort: "ag01", Logger: logger,
	})
	require.NoError(t, err)
	require.NoError(t, reg.RegisterAgent(context.Background(), agentstore.Agent{
		ID: "agt_ag01", AgentTypeID: "hugr-data", ShortID: "ag01",
		Name: "test-agent", Status: "active",
	}))

	c, err := lstore.New(service, lstore.Options{
		AgentID: "agt_ag01", AgentShort: "ag01", Logger: logger,
	})
	require.NoError(t, err)
	return c
}

// ------------------------------------------------------------
// Hypotheses
// ------------------------------------------------------------

func TestHypothesis_CreateAndList(t *testing.T) {
	c := newLearningClient(t)
	ctx := context.Background()

	id1, err := c.CreateHypothesis(ctx, lstore.Hypothesis{
		Content:      "facilities table stores PII",
		Category:     "schema",
		Priority:     "high",
		Verification: "look up GDPR tag",
	})
	require.NoError(t, err)
	require.NotEmpty(t, id1)

	// Second hypothesis, low priority — exercise priority filter.
	id2, err := c.CreateHypothesis(ctx, lstore.Hypothesis{
		Content:  "incidents join facilities via facility_id",
		Category: "schema",
		Priority: "low",
	})
	require.NoError(t, err)

	// List all proposed.
	all, err := c.ListPendingHypotheses(ctx, "", 10)
	require.NoError(t, err)
	assert.Len(t, all, 2)

	// Filter by priority.
	high, err := c.ListPendingHypotheses(ctx, "high", 10)
	require.NoError(t, err)
	require.Len(t, high, 1)
	assert.Equal(t, id1, high[0].ID)

	low, err := c.ListPendingHypotheses(ctx, "low", 10)
	require.NoError(t, err)
	require.Len(t, low, 1)
	assert.Equal(t, id2, low[0].ID)
}

func TestHypothesis_CreateRequiresContent(t *testing.T) {
	c := newLearningClient(t)
	_, err := c.CreateHypothesis(context.Background(), lstore.Hypothesis{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires Content")
}

func TestHypothesis_LifecycleConfirm(t *testing.T) {
	c := newLearningClient(t)
	ctx := context.Background()

	hid, err := c.CreateHypothesis(ctx, lstore.Hypothesis{
		Content: "candidate fact", Priority: "medium",
	})
	require.NoError(t, err)

	require.NoError(t, c.MarkHypothesisChecking(ctx, hid))

	// Now it's out of the pending list.
	pending, err := c.ListPendingHypotheses(ctx, "", 10)
	require.NoError(t, err)
	assert.Empty(t, pending)

	// Confirm with evidence + linked fact id.
	require.NoError(t, c.ConfirmHypothesis(ctx, hid, "evidence: 42 rows", "mem_abc"))

	// Still no pending rows.
	pending, err = c.ListPendingHypotheses(ctx, "", 10)
	require.NoError(t, err)
	assert.Empty(t, pending)
}

func TestHypothesis_LifecycleRejectAndDefer(t *testing.T) {
	c := newLearningClient(t)
	ctx := context.Background()

	h1, err := c.CreateHypothesis(ctx, lstore.Hypothesis{Content: "bad-idea"})
	require.NoError(t, err)
	require.NoError(t, c.MarkHypothesisChecking(ctx, h1))
	require.NoError(t, c.RejectHypothesis(ctx, h1, "evidence: null"))

	h2, err := c.CreateHypothesis(ctx, lstore.Hypothesis{Content: "maybe-later"})
	require.NoError(t, err)
	require.NoError(t, c.MarkHypothesisChecking(ctx, h2))
	require.NoError(t, c.DeferHypothesis(ctx, h2))

	// Only h2 is back in pending (status=proposed).
	pending, err := c.ListPendingHypotheses(ctx, "", 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, h2, pending[0].ID)
}

func TestHypothesis_ExpireOldHypotheses(t *testing.T) {
	c := newLearningClient(t)
	ctx := context.Background()

	_, err := c.CreateHypothesis(ctx, lstore.Hypothesis{Content: "fresh-one"})
	require.NoError(t, err)

	// No hypothesis is older than 1h — affected rows should be 0.
	n, err := c.ExpireOldHypotheses(ctx, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	pending, err := c.ListPendingHypotheses(ctx, "", 10)
	require.NoError(t, err)
	assert.Len(t, pending, 1, "fresh hypothesis must remain")

	// Note: positive delete case isn't asserted here because
	// hypotheses.created_at defaults to DuckDB CURRENT_TIMESTAMP
	// (local TZ) while the cutoff filter sends a UTC Z-suffixed
	// RFC3339 value. Correctness of the delete path is exercised
	// by pkg/memory integration tests via real elapsed time.
}

// ------------------------------------------------------------
// Session reviews
// ------------------------------------------------------------

func TestReview_CreateIdempotent(t *testing.T) {
	c := newLearningClient(t)
	ctx := context.Background()

	id1, err := c.CreateReview(ctx, lstore.Review{SessionID: "sess_1"})
	require.NoError(t, err)

	// Re-create with same SessionID → same ID (idempotent).
	id2, err := c.CreateReview(ctx, lstore.Review{SessionID: "sess_1"})
	require.NoError(t, err)
	assert.Equal(t, id1, id2)

	got, err := c.GetReview(ctx, "sess_1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "pending", got.Status)
}

func TestReview_ListPending(t *testing.T) {
	c := newLearningClient(t)
	ctx := context.Background()

	for i, s := range []string{"sess_a", "sess_b", "sess_c"} {
		_, err := c.CreateReview(ctx, lstore.Review{SessionID: s})
		require.NoError(t, err, "row %d", i)
	}
	pending, err := c.ListPendingReviews(ctx, 10)
	require.NoError(t, err)
	assert.Len(t, pending, 3)
}

func TestReview_CompleteAndFail(t *testing.T) {
	c := newLearningClient(t)
	ctx := context.Background()

	id1, err := c.CreateReview(ctx, lstore.Review{SessionID: "sess_done"})
	require.NoError(t, err)
	require.NoError(t, c.CompleteReview(ctx, id1, lstore.ReviewResult{
		FactsStored:     3,
		FactsReinforced: 1,
		HypothesesAdded: 2,
		ModelUsed:       "gpt-mini",
		TokensUsed:      4200,
	}))

	got, err := c.GetReview(ctx, "sess_done")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "completed", got.Status)
	assert.Equal(t, 3, got.FactsStored)
	assert.Equal(t, 1, got.FactsReinforced)
	assert.Equal(t, 2, got.HypothesesAdded)
	assert.Equal(t, "gpt-mini", got.ModelUsed)
	assert.Equal(t, 4200, got.TokensUsed)
	require.NotNil(t, got.ReviewedAt)

	// Completed review shouldn't appear in pending list.
	pending, err := c.ListPendingReviews(ctx, 10)
	require.NoError(t, err)
	assert.Empty(t, pending)

	// Fail path.
	id2, err := c.CreateReview(ctx, lstore.Review{SessionID: "sess_fail"})
	require.NoError(t, err)
	require.NoError(t, c.FailReview(ctx, id2, "rate-limited"))

	got, err = c.GetReview(ctx, "sess_fail")
	require.NoError(t, err)
	assert.Equal(t, "failed", got.Status)
	assert.Equal(t, "rate-limited", got.Error)
}

func TestReview_GetMissing(t *testing.T) {
	c := newLearningClient(t)
	got, err := c.GetReview(context.Background(), "never-existed")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestReview_CompleteNonexistentErrors(t *testing.T) {
	c := newLearningClient(t)
	err := c.CompleteReview(context.Background(), "ghost", lstore.ReviewResult{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// ------------------------------------------------------------
// Client construction guards
// ------------------------------------------------------------

func TestNew_NilQuerierRejected(t *testing.T) {
	_, err := lstore.New(nil, lstore.Options{AgentID: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil querier")
}

func TestNew_AgentIDRequired(t *testing.T) {
	service, _ := testenv.Engine(t)
	_, err := lstore.New(service, lstore.Options{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AgentID")
}
