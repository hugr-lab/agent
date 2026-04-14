package intent

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

func TestRouter_DefaultModel(t *testing.T) {
	m := &fakeLLM{name: "default-model"}
	r := NewRouter(m)

	assert.Equal(t, "default-model", r.Name())
	assert.Equal(t, m, r.ModelFor(IntentDefault))
	assert.Equal(t, m, r.ModelFor(IntentToolCalling))
	assert.Equal(t, m, r.ModelFor(IntentSummarization))
}

func TestRouter_CustomRoute(t *testing.T) {
	defaultM := &fakeLLM{name: "default"}
	cheapM := &fakeLLM{name: "cheap"}

	r := NewRouter(defaultM)
	r.SetRoute(IntentClassification, cheapM)

	assert.Equal(t, cheapM, r.ModelFor(IntentClassification))
	assert.Equal(t, defaultM, r.ModelFor(IntentDefault))
}

func TestRouter_GenerateContent(t *testing.T) {
	m := &fakeLLM{name: "test"}
	r := NewRouter(m)

	var got string
	for resp, err := range r.GenerateContent(context.Background(), &model.LLMRequest{}, false) {
		assert.NoError(t, err)
		if resp.Content != nil && len(resp.Content.Parts) > 0 {
			got = resp.Content.Parts[0].Text
		}
	}
	assert.Equal(t, "test", got)
}

// fakeConfig implements interfaces.ConfigProvider for testing.
type fakeConfig struct {
	data map[string]string
}

func (c *fakeConfig) Get(key string) any          { return c.data[key] }
func (c *fakeConfig) GetString(key string) string { return c.data[key] }
func (c *fakeConfig) GetInt(key string) int        { return 0 }
func (c *fakeConfig) OnChange(func())              {}

func TestRouter_LoadRoutesFromConfig(t *testing.T) {
	defaultM := &fakeLLM{name: "default"}
	r := NewRouter(defaultM)
	r.WithFactory(func(name string) model.LLM {
		return &fakeLLM{name: name}
	})

	cfg := &fakeConfig{data: map[string]string{
		"llm.routes.default":        "big-model",
		"llm.routes.classification": "tiny-model",
	}}

	r.LoadRoutesFromConfig(cfg)

	assert.Equal(t, "big-model", r.ModelFor(IntentDefault).Name())
	assert.Equal(t, "tiny-model", r.ModelFor(IntentClassification).Name())
	// Unset routes still fall back to the original default.
	assert.Equal(t, "default", r.ModelFor(IntentSummarization).Name())
}

func TestRouter_LoadRoutesFromConfig_NoFactory(t *testing.T) {
	r := NewRouter(&fakeLLM{name: "default"})

	// Should not panic when no factory is set.
	r.LoadRoutesFromConfig(&fakeConfig{data: map[string]string{
		"llm.routes.default": "some-model",
	}})

	assert.Equal(t, "default", r.ModelFor(IntentDefault).Name())
}
