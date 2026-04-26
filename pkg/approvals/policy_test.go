package approvals

import (
	"testing"

	"github.com/stretchr/testify/assert"

	apstore "github.com/hugr-lab/hugen/pkg/approvals/store"
)

// TestParsePolicyDecision verifies wire-form ↔ enum round-trip.
func TestParsePolicyDecision(t *testing.T) {
	cases := []struct {
		in      string
		want    PolicyDecision
		wantErr bool
	}{
		{"always_allowed", PolicyAlwaysAllowed, false},
		{"manual_required", PolicyManualRequired, false},
		{"denied", PolicyDenied, false},
		{"", 0, true},
		{"unknown", 0, true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := ParsePolicyDecision(c.in)
			if c.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, c.want, got)
			assert.Equal(t, c.in, got.String())
		})
	}
}

// TestValidScope covers the scope-grammar guard used by PolicyStore.Set.
func TestValidScope(t *testing.T) {
	cases := []struct {
		scope string
		ok    bool
	}{
		{"global", true},
		{"skill:hugr-data", true},
		{"role:hugr-data:data_analyst", true},
		{"", false},
		{"role:hugr-data", false},        // missing role part
		{"role::data_analyst", false},     // empty skill
		{"role:hugr-data:", false},        // empty role
		{"skill:", false},
		{"agent:foo", false},
	}
	for _, c := range cases {
		t.Run(c.scope, func(t *testing.T) {
			assert.Equal(t, c.ok, validScope(c.scope))
		})
	}
}

// TestBuildSnapshot covers the resolved-row + prefix-entry separation
// + the sort invariants (scope priority ASC, prefix length DESC).
func TestBuildSnapshot(t *testing.T) {
	rows := []apstore.PolicyRecord{
		{ToolName: "data-execute_mutation", Scope: "global", Policy: "manual_required"},
		{ToolName: "data-*", Scope: "global", Policy: "always_allowed"},
		{ToolName: "data-execute_*", Scope: "global", Policy: "manual_required"},
		{ToolName: "memory_note", Scope: "skill:_memory", Policy: "denied"},
		{ToolName: "memory_*", Scope: "role:_memory:reviewer", Policy: "always_allowed"},
		{ToolName: "garbage", Scope: "global", Policy: "BOGUS"}, // skipped silently
	}
	snap, err := buildSnapshot(rows)
	assert.NoError(t, err)

	// Exact map: 2 entries (the two non-glob rows; the bogus one
	// dropped during parse).
	assert.Len(t, snap.exact, 2)
	if r, ok := snap.exact[snapshotKey{"global", "data-execute_mutation"}]; ok {
		assert.Equal(t, PolicyManualRequired, r.Decision)
	}
	if r, ok := snap.exact[snapshotKey{"skill:_memory", "memory_note"}]; ok {
		assert.Equal(t, PolicyDenied, r.Decision)
	}

	// Prefix slice: 3 entries (data-*, data-execute_*, memory_*),
	// sorted: role-scoped first, then global; within global, the
	// LONGER prefix data-execute_ wins ahead of data-.
	assert.Len(t, snap.prefix, 3)
	assert.Equal(t, "role:_memory:reviewer", snap.prefix[0].Scope)
	assert.Equal(t, "memory_", snap.prefix[0].Prefix)
	assert.Equal(t, "global", snap.prefix[1].Scope)
	assert.Equal(t, "data-execute_", snap.prefix[1].Prefix)
	assert.Equal(t, "global", snap.prefix[2].Scope)
	assert.Equal(t, "data-", snap.prefix[2].Prefix)
}

// TestPolicyStore_Resolve_ChainOrder exercises the full chain
// priority: role > skill > global > frontmatter > default.
func TestPolicyStore_Resolve_ChainOrder(t *testing.T) {
	rows := []apstore.PolicyRecord{
		// global manual on data-execute_mutation
		{Scope: "global", ToolName: "data-execute_mutation", Policy: "manual_required"},
		// skill scope overrides to allowed for hugr-data
		{Scope: "skill:hugr-data", ToolName: "data-execute_mutation", Policy: "always_allowed"},
		// role scope overrides back to denied for the analyst role
		{Scope: "role:hugr-data:data_analyst", ToolName: "data-execute_mutation", Policy: "denied"},
	}
	snap, err := buildSnapshot(rows)
	assert.NoError(t, err)

	ps := &PolicyStore{}
	ps.snapshot.Store(snap)

	t.Run("role exact wins over skill + global", func(t *testing.T) {
		got := ps.Resolve(nil, ToolCall{
			ToolName: "data-execute_mutation",
			Skill:    "hugr-data",
			Role:     "data_analyst",
		})
		assert.Equal(t, PolicyDenied, got.Policy)
		assert.Contains(t, got.Origin, "role:hugr-data:data_analyst")
	})

	t.Run("skill wins when role unset", func(t *testing.T) {
		got := ps.Resolve(nil, ToolCall{
			ToolName: "data-execute_mutation",
			Skill:    "hugr-data",
		})
		assert.Equal(t, PolicyAlwaysAllowed, got.Policy)
		assert.Contains(t, got.Origin, "skill:hugr-data")
	})

	t.Run("global is the only match without skill/role", func(t *testing.T) {
		got := ps.Resolve(nil, ToolCall{ToolName: "data-execute_mutation"})
		assert.Equal(t, PolicyManualRequired, got.Policy)
		assert.Contains(t, got.Origin, "global")
	})
}

// TestPolicyStore_Resolve_PrefixGlobs covers prefix-match behavior.
func TestPolicyStore_Resolve_PrefixGlobs(t *testing.T) {
	rows := []apstore.PolicyRecord{
		{Scope: "global", ToolName: "data-*", Policy: "manual_required"},
		{Scope: "global", ToolName: "data-execute_*", Policy: "denied"},
	}
	snap, err := buildSnapshot(rows)
	assert.NoError(t, err)
	ps := &PolicyStore{}
	ps.snapshot.Store(snap)

	t.Run("longer prefix wins", func(t *testing.T) {
		got := ps.Resolve(nil, ToolCall{ToolName: "data-execute_mutation"})
		assert.Equal(t, PolicyDenied, got.Policy)
	})

	t.Run("shorter prefix matches when longer doesn't", func(t *testing.T) {
		got := ps.Resolve(nil, ToolCall{ToolName: "data-list_modules"})
		assert.Equal(t, PolicyManualRequired, got.Policy)
	})

	t.Run("unrelated tool falls through to default", func(t *testing.T) {
		got := ps.Resolve(nil, ToolCall{ToolName: "memory_note"})
		assert.Equal(t, PolicyAlwaysAllowed, got.Policy)
		assert.Equal(t, OriginDefault, got.Source)
	})
}

// TestPolicyStore_Resolve_FrontmatterFallback ensures frontmatter
// rules apply when neither cache nor prefix matched, but the
// hardcoded default (step 8) still wins when there's no frontmatter
// match either.
func TestPolicyStore_Resolve_FrontmatterFallback(t *testing.T) {
	ps := &PolicyStore{}
	ps.snapshot.Store(&policySnapshot{exact: map[snapshotKey]ResolvedRow{}, prefix: nil})

	fm := &FrontmatterApprovalRules{
		RequireUser: []string{"data-execute_mutation", "data-delete_*"},
		AutoApprove: []string{"discovery-*", "schema-*"},
	}

	t.Run("frontmatter require_user", func(t *testing.T) {
		got := ps.Resolve(nil, ToolCall{ToolName: "data-execute_mutation", Frontmatter: fm})
		assert.Equal(t, PolicyManualRequired, got.Policy)
		assert.Equal(t, OriginFrontmatter, got.Source)
		assert.Equal(t, RiskMedium, got.Risk)
	})

	t.Run("frontmatter prefix glob", func(t *testing.T) {
		got := ps.Resolve(nil, ToolCall{ToolName: "data-delete_old", Frontmatter: fm})
		assert.Equal(t, PolicyManualRequired, got.Policy)
	})

	t.Run("frontmatter auto_approve", func(t *testing.T) {
		got := ps.Resolve(nil, ToolCall{ToolName: "discovery-search", Frontmatter: fm})
		assert.Equal(t, PolicyAlwaysAllowed, got.Policy)
		assert.Equal(t, OriginFrontmatter, got.Source)
	})

	t.Run("frontmatter risk_overrides", func(t *testing.T) {
		fmHigh := &FrontmatterApprovalRules{
			RequireUser:   []string{"data-execute_mutation"},
			RiskOverrides: map[string]Risk{"data-execute_mutation": RiskHigh},
		}
		got := ps.Resolve(nil, ToolCall{ToolName: "data-execute_mutation", Frontmatter: fmHigh})
		assert.Equal(t, RiskHigh, got.Risk)
	})

	t.Run("no frontmatter match falls to default", func(t *testing.T) {
		got := ps.Resolve(nil, ToolCall{ToolName: "memory_note", Frontmatter: fm})
		assert.Equal(t, PolicyAlwaysAllowed, got.Policy)
		assert.Equal(t, OriginDefault, got.Source)
	})
}
