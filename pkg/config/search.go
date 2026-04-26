package config

import "time"

// SearchConfig bundles operator knobs for the multi-horizon
// session-context search subsystem (spec 009 phase 4). Read by the
// session_context tool body.
type SearchConfig struct {
	// DefaultLimit is the result-row cap when the caller does not
	// pass `last_n`. Zero falls back to 20. The Hugr `semantic:`
	// argument is requested with `limit * 3` headroom for Go-side
	// recency reranking; this knob is the post-rerank truncation
	// limit, not the GraphQL limit.
	DefaultLimit int `mapstructure:"default_limit"`

	// DefaultHalfLifeMission is the recency-decay half-life applied
	// when scope=mission and the caller does not pass `half_life`.
	// Zero falls back to 1h.
	DefaultHalfLifeMission time.Duration `mapstructure:"default_half_life_mission"`

	// DefaultHalfLifeSession is the recency-decay half-life applied
	// when scope=session and the caller does not pass `half_life`.
	// Zero falls back to 24h.
	DefaultHalfLifeSession time.Duration `mapstructure:"default_half_life_session"`

	// DefaultHalfLifeUser is the recency-decay half-life applied
	// when scope=user and the caller does not pass `half_life`.
	// Zero falls back to 168h (one week).
	DefaultHalfLifeUser time.Duration `mapstructure:"default_half_life_user"`

	// UserBatchAliasLimit is the threshold at which `scope: user`
	// stops batching root sessions into a single GraphQL document
	// (one aliased block per root) and instead falls back to a
	// sequential per-root query stream with a worker pool. Lower
	// values cap the GraphQL document size; higher values reduce
	// round-trip overhead. Zero falls back to 50.
	UserBatchAliasLimit int `mapstructure:"user_batch_alias_limit"`
}

// applySearchDefaults fills zero-valued SearchConfig fields with
// their documented defaults. Called from LoadLocal +
// decodeAndFinalize after YAML unmarshal so operator overrides
// survive but missing keys land on safe values.
func applySearchDefaults(c *SearchConfig) {
	if c.DefaultLimit == 0 {
		c.DefaultLimit = 20
	}
	if c.DefaultHalfLifeMission == 0 {
		c.DefaultHalfLifeMission = 1 * time.Hour
	}
	if c.DefaultHalfLifeSession == 0 {
		c.DefaultHalfLifeSession = 24 * time.Hour
	}
	if c.DefaultHalfLifeUser == 0 {
		c.DefaultHalfLifeUser = 168 * time.Hour
	}
	if c.UserBatchAliasLimit == 0 {
		c.UserBatchAliasLimit = 50
	}
}
