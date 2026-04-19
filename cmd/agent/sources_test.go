//go:build duckdb_arrow

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTypePredicates(t *testing.T) {
	assert.True(t, isLLMType("llm-openai"))
	assert.True(t, isLLMType("llm-anthropic"))
	assert.True(t, isLLMType("llm-gemini"))
	assert.False(t, isLLMType("embedding"))
	assert.False(t, isLLMType("postgres"))

	assert.True(t, isEmbeddingType("embedding"))
	assert.False(t, isEmbeddingType("llm-openai"))
	assert.False(t, isEmbeddingType(""))
}
