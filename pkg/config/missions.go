package config

import "time"

// MissionsConfig bundles the operator knobs for the mission-graph
// runtime. All zero-values are safe defaults (router + executor
// fall back to their internal defaults when a field is unset).
type MissionsConfig struct {
	// FollowUpEnabled flips the follow-up router on/off. When false
	// the BeforeModelCallback proceeds unconditionally and every
	// user message takes the normal coordinator-plans-from-scratch
	// path, regardless of running missions.
	FollowUpEnabled bool `mapstructure:"follow_up_enabled"`

	// FollowUpSimilarityThreshold is the classifier-confidence floor
	// for accepting a match. Values below threshold always proceed
	// without routing. 0 falls back to 0.55.
	FollowUpSimilarityThreshold float64 `mapstructure:"follow_up_similarity_threshold"`

	// FollowUpTieBand is the ambiguity window above the threshold.
	// Match confidence must exceed Threshold+TieBand to route;
	// otherwise the router treats the situation as too close to call
	// and proceeds. 0 falls back to 0.05.
	FollowUpTieBand float64 `mapstructure:"follow_up_tie_band"`

	// ClassifierTimeout caps the single classifier LLM call made per
	// eligible user message. 0 falls back to 3s.
	ClassifierTimeout time.Duration `mapstructure:"classifier_timeout"`

	// StaleMissionTimeout is the cutoff Restore uses to decide
	// whether an active sub-agent session is dead after a restart:
	// if `now - last_event > StaleMissionTimeout` the mission is
	// marked abandoned with reason="restart: stale". 0 falls back
	// to 5m. Trade-off: too short → live but slow missions get
	// killed on every restart; too long → operators wait minutes
	// before stuck rows are reaped.
	StaleMissionTimeout time.Duration `mapstructure:"stale_mission_timeout"`
}
