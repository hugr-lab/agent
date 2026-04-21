package models

// Config holds LLM routing + default-model tuning read from `llm:` in
// config.yaml. Owned by pkg/models because the router + hugr-LLM
// adapter are the concrete consumers.
//
// Routes is intent-name → model-name; unmatched intents fall back to
// the default model (Model field).
type Config struct {
	Model       string            `mapstructure:"model"`
	MaxTokens   int               `mapstructure:"max_tokens"`
	Temperature float32           `mapstructure:"temperature"`
	Routes      map[string]string `mapstructure:"routes"`
}
