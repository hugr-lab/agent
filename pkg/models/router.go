package models

import (
	"context"
	"iter"
	"log/slog"
	"sync"

	"google.golang.org/adk/model"
)

// ConfigProvider is the minimal keyed-config view needed by
// LoadRoutesFromConfig. Any source that can return a string by key
// satisfies it.
type ConfigProvider interface {
	GetString(key string) string
}

// ModelFactory creates a model.LLM from a Hugr data source name.
// Used by Router to instantiate models from config-driven route names.
type ModelFactory func(hugrModelName string) model.LLM

// Router wraps a base model.LLM and routes requests by intent.
// Thread-safe: routes can be updated via LoadRoutesFromConfig (called
// from file watcher goroutine) while GenerateContent runs concurrently.
type Router struct {
	mu           sync.RWMutex
	defaultModel model.LLM
	routes       map[Intent]model.LLM
	factory      ModelFactory
	logger       *slog.Logger
}

// NewRouter creates an IntentLLM router. All intents initially route to the default model.
func NewRouter(defaultModel model.LLM) *Router {
	return &Router{
		defaultModel: defaultModel,
		routes:       make(map[Intent]model.LLM),
		logger:       slog.Default(),
	}
}

// WithFactory sets a model factory for config-driven route changes.
func (r *Router) WithFactory(f ModelFactory) *Router {
	r.factory = f
	return r
}

// WithLogger sets the logger.
func (r *Router) WithLogger(l *slog.Logger) *Router {
	r.logger = l
	return r
}

// SetRoute maps an intent to a specific model.
func (r *Router) SetRoute(intent Intent, m model.LLM) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes[intent] = m
}

// ModelFor returns the model mapped to the given intent, falling back to default.
func (r *Router) ModelFor(intent Intent) model.LLM {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if m, ok := r.routes[intent]; ok {
		return m
	}
	return r.defaultModel
}

// LoadRoutesFromConfig reads llm.routes from the ConfigProvider and sets up
// model routing. Called at startup and on config change.
func (r *Router) LoadRoutesFromConfig(cfg ConfigProvider) {
	if r.factory == nil || cfg == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	intents := []Intent{IntentDefault, IntentToolCalling, IntentSummarization, IntentClassification}
	for _, intent := range intents {
		key := "llm.routes." + string(intent)
		name := cfg.GetString(key)
		if name == "" {
			continue
		}
		m := r.factory(name)
		r.routes[intent] = m
		r.logger.Info("intent route configured", "intent", string(intent), "model", name)
	}
}

// model.LLM interface — delegates via ModelFor(IntentDefault).
func (r *Router) Name() string {
	return r.ModelFor(IntentDefault).Name()
}

func (r *Router) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return r.ModelFor(IntentDefault).GenerateContent(ctx, req, stream)
}
