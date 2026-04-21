package chatcontext

// Config holds chat-context YAML tuning read from `chatcontext:` in
// config.yaml. CompactionThreshold is the context-window budget
// fraction (0..1) at which the Compactor BeforeModelCallback starts
// summarising old turns.
type Config struct {
	CompactionThreshold float64 `mapstructure:"compaction_threshold"`
}
