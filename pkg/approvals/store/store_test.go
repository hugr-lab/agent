//go:build duckdb_arrow

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hugr-lab/hugen/internal/testenv"
	"github.com/hugr-lab/hugen/pkg/approvals/store"
)

const testAgentID = "agt_ag01"

func newClient(t *testing.T) (*store.Client, context.Context) {
	t.Helper()
	svc, _ := testenv.SharedEngine()
	require.NotNil(t, svc, "testenv.SharedEngine returned nil")
	ctx := context.Background()
	require.NoError(t, resetSharedTables(t, ctx, svc))

	c, err := store.New(svc, store.Options{AgentID: testAgentID})
	require.NoError(t, err)
	return c, ctx
}

// TestApprovals_InsertGetList exercises the happy path: insert one
// row, get it back, list it, verify field round-trip.
func TestApprovals_InsertGetList(t *testing.T) {
	c, ctx := newClient(t)

	rec := store.ApprovalRecord{
		ID:               "app-test-001",
		AgentID:          testAgentID,
		MissionSessionID: "sess_mis_test",
		CoordSessionID:   "sess_coord_test",
		ToolName:         "data-execute_mutation",
		Args:             map[string]any{"statement": "DELETE FROM x"},
		Risk:             "high",
		Status:           "pending",
	}
	require.NoError(t, c.InsertApproval(ctx, rec))

	got, err := c.GetApproval(ctx, rec.ID)
	require.NoError(t, err)
	assert.Equal(t, rec.ID, got.ID)
	assert.Equal(t, rec.MissionSessionID, got.MissionSessionID)
	assert.Equal(t, rec.CoordSessionID, got.CoordSessionID)
	assert.Equal(t, rec.ToolName, got.ToolName)
	assert.Equal(t, rec.Risk, got.Risk)
	assert.Equal(t, rec.Status, got.Status)
	require.NotNil(t, got.Args)
	assert.Equal(t, "DELETE FROM x", got.Args["statement"])
	assert.Nil(t, got.Response)
	assert.Nil(t, got.RespondedAt)

	// List with default filter (pending only).
	rows, err := c.ListApprovals(ctx, store.ListFilter{Statuses: []string{"pending"}})
	require.NoError(t, err)
	assert.Len(t, rows, 1)
	assert.Equal(t, rec.ID, rows[0].ID)
}

// TestApprovals_GetMissing verifies ErrApprovalNotFound on missing id.
func TestApprovals_GetMissing(t *testing.T) {
	c, ctx := newClient(t)

	_, err := c.GetApproval(ctx, "app-does-not-exist")
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrApprovalNotFound), "want ErrApprovalNotFound, got: %v", err)
}

// TestApprovals_UpdateStatus_PendingTransitions verifies a pending
// row transitions to approved + carries response payload.
func TestApprovals_UpdateStatus_PendingTransitions(t *testing.T) {
	c, ctx := newClient(t)

	require.NoError(t, c.InsertApproval(ctx, store.ApprovalRecord{
		ID: "app-test-002", AgentID: testAgentID,
		MissionSessionID: "m1", CoordSessionID: "c1",
		ToolName: "x", Args: map[string]any{}, Risk: "medium", Status: "pending",
	}))

	now := time.Now().UTC()
	resp := map[string]any{"decision": "approve", "note": "ok"}
	require.NoError(t, c.UpdateStatus(ctx, "app-test-002", "approved", resp, now))

	got, err := c.GetApproval(ctx, "app-test-002")
	require.NoError(t, err)
	assert.Equal(t, "approved", got.Status)
	require.NotNil(t, got.Response)
	assert.Equal(t, "approve", got.Response["decision"])
	assert.Equal(t, "ok", got.Response["note"])
	require.NotNil(t, got.RespondedAt)
}

// TestApprovals_UpdateStatus_AlreadyResolved verifies the second
// transition attempt errors cleanly.
func TestApprovals_UpdateStatus_AlreadyResolved(t *testing.T) {
	c, ctx := newClient(t)

	require.NoError(t, c.InsertApproval(ctx, store.ApprovalRecord{
		ID: "app-test-003", AgentID: testAgentID,
		MissionSessionID: "m1", CoordSessionID: "c1",
		ToolName: "x", Args: map[string]any{}, Risk: "low", Status: "pending",
	}))
	now := time.Now().UTC()
	require.NoError(t, c.UpdateStatus(ctx, "app-test-003", "approved", nil, now))

	err := c.UpdateStatus(ctx, "app-test-003", "rejected", nil, now)
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrAlreadyResolved), "want ErrAlreadyResolved, got: %v", err)
}

// TestApprovals_SweepExpired verifies aged pending rows transition
// to expired.
func TestApprovals_SweepExpired(t *testing.T) {
	c, ctx := newClient(t)

	require.NoError(t, c.InsertApproval(ctx, store.ApprovalRecord{
		ID: "app-test-old", AgentID: testAgentID,
		MissionSessionID: "m1", CoordSessionID: "c1",
		ToolName: "x", Args: map[string]any{}, Risk: "low", Status: "pending",
	}))
	require.NoError(t, c.InsertApproval(ctx, store.ApprovalRecord{
		ID: "app-test-new", AgentID: testAgentID,
		MissionSessionID: "m1", CoordSessionID: "c1",
		ToolName: "y", Args: map[string]any{}, Risk: "low", Status: "pending",
	}))

	// Cutoff: future moment so the OLD row is "before" cutoff but
	// NEW row's created_at is also before cutoff. We want both to
	// expire here just for round-trip; precise time-based filtering
	// is exercised in higher-level Manager tests.
	cutoff := time.Now().Add(1 * time.Hour)
	expired, err := c.SweepExpired(ctx, cutoff)
	require.NoError(t, err)
	assert.Len(t, expired, 2, "expected both pending rows to be swept")

	// Both rows should now be terminal.
	for _, id := range []string{"app-test-old", "app-test-new"} {
		got, err := c.GetApproval(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, "expired", got.Status)
		assert.NotNil(t, got.RespondedAt)
	}
}

// TestPolicies_UpsertGetDelete exercises the policy CRUD round-trip.
func TestPolicies_UpsertGetDelete(t *testing.T) {
	c, ctx := newClient(t)

	p := store.PolicyRecord{
		AgentID:   testAgentID,
		ToolName:  "data-execute_mutation",
		Scope:     "global",
		Policy:    "manual_required",
		Note:      "destructive",
		CreatedBy: "user",
	}
	require.NoError(t, c.UpsertPolicy(ctx, p))

	got, err := c.GetPolicy(ctx, testAgentID, p.ToolName, p.Scope)
	require.NoError(t, err)
	assert.Equal(t, "manual_required", got.Policy)
	assert.Equal(t, "destructive", got.Note)
	assert.Equal(t, "user", got.CreatedBy)

	// Upsert with the same key + new policy = update.
	p.Policy = "always_allowed"
	p.Note = "user widened it"
	require.NoError(t, c.UpsertPolicy(ctx, p))

	got, err = c.GetPolicy(ctx, testAgentID, p.ToolName, p.Scope)
	require.NoError(t, err)
	assert.Equal(t, "always_allowed", got.Policy)
	assert.Equal(t, "user widened it", got.Note)

	// LoadAll returns everything.
	all, err := c.LoadAllPolicies(ctx)
	require.NoError(t, err)
	assert.Len(t, all, 1)

	// Delete + verify gone.
	existed, err := c.DeletePolicy(ctx, testAgentID, p.ToolName, p.Scope)
	require.NoError(t, err)
	assert.True(t, existed)

	_, err = c.GetPolicy(ctx, testAgentID, p.ToolName, p.Scope)
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrPolicyNotFound))

	// Delete on missing row returns existed=false without error.
	existed, err = c.DeletePolicy(ctx, testAgentID, p.ToolName, p.Scope)
	require.NoError(t, err)
	assert.False(t, existed)
}
