package runtime

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestApplyApprovalsDefaults verifies that zero-valued ApprovalsConfig
// fields land on the documented defaults — and that operator-supplied
// non-zero values are preserved.
func TestApplyApprovalsDefaults(t *testing.T) {
	t.Run("all-zero falls back to documented defaults", func(t *testing.T) {
		c := ApprovalsConfig{}
		applyApprovalsDefaults(&c)

		assert.Equal(t, 30*time.Minute, c.DefaultTimeout)
		assert.Equal(t, 5*time.Minute, c.SweeperInterval)
		require.NotNil(t, c.SafePolicyChange)
		assert.True(t, *c.SafePolicyChange)
		assert.False(t, c.EnableImpactEstimators) // zero-value is the documented default
		assert.Empty(t, c.DestructiveTools)
	})

	t.Run("operator overrides survive", func(t *testing.T) {
		falseVal := false
		c := ApprovalsConfig{
			DefaultTimeout:         15 * time.Minute,
			SweeperInterval:        2 * time.Minute,
			SafePolicyChange:       &falseVal,
			EnableImpactEstimators: true,
			DestructiveTools:       []string{"data-execute_mutation"},
		}
		applyApprovalsDefaults(&c)

		assert.Equal(t, 15*time.Minute, c.DefaultTimeout)
		assert.Equal(t, 2*time.Minute, c.SweeperInterval)
		require.NotNil(t, c.SafePolicyChange)
		assert.False(t, *c.SafePolicyChange, "operator-set false must not be flipped to true")
		assert.True(t, c.EnableImpactEstimators)
		assert.Equal(t, []string{"data-execute_mutation"}, c.DestructiveTools)
	})
}

// TestApplySearchDefaults verifies that zero-valued SearchConfig fields
// land on the documented defaults — and that operator-supplied non-zero
// values are preserved.
func TestApplySearchDefaults(t *testing.T) {
	t.Run("all-zero falls back to documented defaults", func(t *testing.T) {
		c := SearchConfig{}
		applySearchDefaults(&c)

		assert.Equal(t, 20, c.DefaultLimit)
		assert.Equal(t, 1*time.Hour, c.DefaultHalfLifeMission)
		assert.Equal(t, 24*time.Hour, c.DefaultHalfLifeSession)
		assert.Equal(t, 168*time.Hour, c.DefaultHalfLifeUser)
		assert.Equal(t, 50, c.UserBatchAliasLimit)
	})

	t.Run("operator overrides survive", func(t *testing.T) {
		c := SearchConfig{
			DefaultLimit:           10,
			DefaultHalfLifeMission: 30 * time.Minute,
			DefaultHalfLifeSession: 2 * time.Hour,
			DefaultHalfLifeUser:    72 * time.Hour,
			UserBatchAliasLimit:    100,
		}
		applySearchDefaults(&c)

		assert.Equal(t, 10, c.DefaultLimit)
		assert.Equal(t, 30*time.Minute, c.DefaultHalfLifeMission)
		assert.Equal(t, 2*time.Hour, c.DefaultHalfLifeSession)
		assert.Equal(t, 72*time.Hour, c.DefaultHalfLifeUser)
		assert.Equal(t, 100, c.UserBatchAliasLimit)
	})
}

// TestLoadLocal_AppliesPhase4Defaults_WithoutYAML verifies the
// pure-env / no-YAML path (used by tests + minimal deployments)
// applies phase-4 defaults to Approvals + Search.
func TestLoadLocal_AppliesPhase4Defaults_WithoutYAML(t *testing.T) {
	boot := &BootstrapConfig{}
	cfg, err := LoadLocal("", boot, nil)
	require.NoError(t, err)

	assert.Equal(t, 30*time.Minute, cfg.Approvals.DefaultTimeout)
	assert.Equal(t, 5*time.Minute, cfg.Approvals.SweeperInterval)
	require.NotNil(t, cfg.Approvals.SafePolicyChange)
	assert.True(t, *cfg.Approvals.SafePolicyChange)

	assert.Equal(t, 20, cfg.Search.DefaultLimit)
	assert.Equal(t, 1*time.Hour, cfg.Search.DefaultHalfLifeMission)
	assert.Equal(t, 24*time.Hour, cfg.Search.DefaultHalfLifeSession)
	assert.Equal(t, 168*time.Hour, cfg.Search.DefaultHalfLifeUser)
	assert.Equal(t, 50, cfg.Search.UserBatchAliasLimit)
}
