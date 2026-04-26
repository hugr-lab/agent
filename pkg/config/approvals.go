package config

import "time"

// ApprovalsConfig bundles operator knobs for the HITL approval
// subsystem (spec 009 phase 4). The approvals manager + Gate read
// these settings; sub-agent code never does.
type ApprovalsConfig struct {
	// DefaultTimeout is how long a pending approval row stays in
	// `pending` before the timeout sweeper marks it `expired` and
	// fails the corresponding mission. Zero falls back to 30m.
	DefaultTimeout time.Duration `mapstructure:"default_timeout"`

	// SweeperInterval is the cron cadence for the timeout sweeper.
	// Each tick runs one bulk UPDATE over `approvals` past their
	// timeout. Zero falls back to 5m.
	SweeperInterval time.Duration `mapstructure:"sweeper_interval"`

	// SafePolicyChange controls whether a `policy_set` call that
	// would WIDEN permissions on a tool currently resolving to
	// `manual_required` (e.g. setting it to `always_allowed`) itself
	// triggers `require_user`. The user must explicitly approve
	// the policy change before it takes effect. This defuses
	// prompt-injection attacks aimed at silencing the gate.
	// Zero/unset falls back to true.
	SafePolicyChange *bool `mapstructure:"safe_policy_change"`

	// EnableImpactEstimators turns on per-tool impact estimators
	// that populate `estimated_impact` on approval envelopes
	// (e.g. `EXPLAIN`-driven row counts for `data-execute_mutation`).
	// Adds a Hugr round-trip per gated call, so default off.
	EnableImpactEstimators bool `mapstructure:"enable_impact_estimators"`

	// DestructiveTools is the operator-managed list of tool names
	// that fall through the policy + approval-rules chain to a
	// hardcoded default of `manual_required` (instead of the usual
	// `auto_approve`). Default empty.
	DestructiveTools []string `mapstructure:"destructive_tools"`
}

// applyApprovalsDefaults fills zero-valued ApprovalsConfig fields
// with their documented defaults. Called from LoadLocal +
// decodeAndFinalize after YAML unmarshal so operator overrides
// survive but missing keys land on safe values.
func applyApprovalsDefaults(c *ApprovalsConfig) {
	if c.DefaultTimeout == 0 {
		c.DefaultTimeout = 30 * time.Minute
	}
	if c.SweeperInterval == 0 {
		c.SweeperInterval = 5 * time.Minute
	}
	if c.SafePolicyChange == nil {
		t := true
		c.SafePolicyChange = &t
	}
}
