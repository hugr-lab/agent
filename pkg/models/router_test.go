package models

import (
	"context"
	"iter"
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

type fakeLLM struct{ name string }

func (f *fakeLLM) Name() string { return f.name }
func (f *fakeLLM) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content:      &genai.Content{Parts: []*genai.Part{{Text: f.name}}},
			TurnComplete: true,
		}, nil)
	}
}

// newTestRouter builds a Router with a pre-built default model — bypasses
// NewRouter's querier-based construction so tests don't need to stand
// up a Hugr adapter.
func newTestRouter(m model.LLM) *Router {
	r := &Router{
		defaultModel: m,
		routes:       make(map[Intent]model.LLM),
	}
	r.WithLogger(nil)
	return r
}

func TestRouter_DefaultModel(t *testing.T) {
	m := &fakeLLM{name: "default-model"}
	r := newTestRouter(m)

	assert.Equal(t, "default-model", r.Name())
	assert.Equal(t, m, r.ModelFor(IntentDefault))
	assert.Equal(t, m, r.ModelFor(IntentToolCalling))
	assert.Equal(t, m, r.ModelFor(IntentSummarization))
}

func TestRouter_CustomRoute(t *testing.T) {
	defaultM := &fakeLLM{name: "default"}
	cheapM := &fakeLLM{name: "cheap"}

	r := newTestRouter(defaultM)
	r.SetRoute(IntentClassification, cheapM)

	assert.Equal(t, cheapM, r.ModelFor(IntentClassification))
	assert.Equal(t, defaultM, r.ModelFor(IntentDefault))
}

func TestRouter_GenerateContent(t *testing.T) {
	m := &fakeLLM{name: "test"}
	r := newTestRouter(m)

	var got string
	for resp, err := range r.GenerateContent(context.Background(), &model.LLMRequest{}, false) {
		assert.NoError(t, err)
		if resp.Content != nil && len(resp.Content.Parts) > 0 {
			got = resp.Content.Parts[0].Text
		}
	}
	assert.Equal(t, "test", got)
}
