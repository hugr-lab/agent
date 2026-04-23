package models

// Intent classifies the current LLM task for model routing.
type Intent string

const (
	// IntentDefault is for general reasoning and user interaction.
	IntentDefault Intent = "default"

	// IntentToolCalling is for tool selection and execution (cheap model for sub-agents).
	IntentToolCalling Intent = "tool_calling"

	// IntentSummarization is for context compaction and summaries (cheap model).
	IntentSummarization Intent = "summarization"

	// IntentClassification is for intent/category detection (cheap model).
	IntentClassification Intent = "classification"
)
