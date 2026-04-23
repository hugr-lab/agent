// Package models owns the agent's LLM adapters and intent-based
// router. The Router resolves an Intent to a concrete model.LLM from
// a pool of providers (hugr-hosted or native SDKs), exposes a
// TokenEstimator for prompt-size feedback, and provides scripted test
// doubles for deterministic unit tests.
package models

import (
	"context"
	"iter"
	"log/slog"
	"sync"

	"github.com/hugr-lab/query-engine/types"
	"google.golang.org/adk/model"
)

// Router wraps model.LLM instances and routes requests by intent.
// Router owns LLM construction: per model it picks a querier (local
// engine for models registered in local, remote hugr otherwise), and
// builds a model.LLM via NewHugr with the shared options.
//
// Thread-safe: routes are populated once at NewRouter and never
// mutated after — no locking needed on read.
type Router struct {
	mu           sync.RWMutex
	defaultModel model.LLM
	routes       map[Intent]model.LLM
	logger       *slog.Logger
}

// NewRouter constructs a Router given both possible queriers, the
// list of model names that live in the local engine, the LLM config
// (default Model + per-intent Routes + shared MaxTokens/Temperature),
// and a list of construction Options appended to every built model.
//
// remote is always required (default fallback querier).
// local may be nil when LocalDB is disabled — localModels must then
// be empty.
func NewRouter(
	local types.Querier,
	remote types.Querier,
	localModels []string,
	cfg Config,
	opts ...Option,
) *Router {
	r := &Router{
		routes: make(map[Intent]model.LLM),
		logger: slog.Default(),
	}

	isLocal := make(map[string]bool, len(localModels))
	for _, n := range localModels {
		isLocal[n] = true
	}
	querierFor := func(modelName string) types.Querier {
		if local != nil && isLocal[modelName] {
			return local
		}
		return remote
	}

	// Shared model options: attach MaxTokens / Temperature from cfg in
	// addition to the caller-supplied ones.
	buildOpts := func() []Option {
		out := make([]Option, 0, len(opts)+2)
		out = append(out, opts...)
		if cfg.MaxTokens > 0 {
			out = append(out, WithMaxTokens(cfg.MaxTokens))
		}
		if cfg.Temperature > 0 {
			out = append(out, WithTemperature(cfg.Temperature))
		}
		return out
	}

	if cfg.Model != "" {
		r.defaultModel = NewHugr(querierFor(cfg.Model), cfg.Model, buildOpts()...)
	}

	for intentName, modelName := range cfg.Routes {
		if modelName == "" {
			continue
		}
		m := NewHugr(querierFor(modelName), modelName, buildOpts()...)
		r.routes[Intent(intentName)] = m
	}
	return r
}

// NewRouterWithDefault builds a Router wrapping a pre-constructed
// default model. Handy for tests + for callers that don't need per-
// model querier routing.
func NewRouterWithDefault(defaultModel model.LLM) *Router {
	return &Router{
		defaultModel: defaultModel,
		routes:       make(map[Intent]model.LLM),
		logger:       slog.Default(),
	}
}

// WithLogger sets the logger.
func (r *Router) WithLogger(l *slog.Logger) *Router {
	r.logger = l
	return r
}

// ModelFor returns the model mapped to the given intent, falling back
// to the default model when no explicit route is set.
func (r *Router) ModelFor(intent Intent) model.LLM {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if m, ok := r.routes[intent]; ok {
		return m
	}
	return r.defaultModel
}

// SetRoute — still exposed for tests that inject fake LLMs directly.
// Production wiring uses NewRouter alone.
func (r *Router) SetRoute(intent Intent, m model.LLM) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes[intent] = m
}

// model.LLM interface — delegates via ModelFor(IntentDefault).
func (r *Router) Name() string {
	return r.ModelFor(IntentDefault).Name()
}

func (r *Router) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return r.ModelFor(IntentDefault).GenerateContent(ctx, req, stream)
}
