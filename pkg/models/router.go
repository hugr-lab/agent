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

// budgetFloor is the last-resort context budget when neither
// Config.ContextWindows nor Config.DefaultBudget supplies a value.
// Matches the historical hard-coded threshold the compactor used
// before spec 006.
const budgetFloor = 128_000

// Router wraps model.LLM instances and routes requests by intent.
// Router owns LLM construction: per model it picks a querier (local
// engine for models registered in local, remote hugr otherwise), and
// builds a model.LLM via NewHugr with the shared options.
//
// Routes + budgets are populated once at NewRouter and never mutated
// after; concurrent reads are safe without locking. SetRoute (used by
// tests) is the only path that mutates after construction and takes
// the write lock.
//
// Spec 006: BudgetFor(intent) returns the input-context capacity for
// the model the router resolves to for `intent`. Resolution chain:
//
//  1. ContextWindows[<model name>] when present and > 0
//  2. Config.DefaultBudget when > 0
//  3. budgetFloor (with a one-shot INFO log per intent)
type Router struct {
	mu               sync.RWMutex
	defaultModel     model.LLM
	defaultModelName string
	routeNames       map[Intent]string
	routes           map[Intent]model.LLM
	contextWindows   map[string]int
	defaultBudget    int

	// budgetWarn tracks intents that already produced the floor-fallback
	// log line; ensures one INFO per intent over the router's lifetime.
	budgetWarnMu sync.Mutex
	budgetWarn   map[Intent]struct{}

	logger *slog.Logger
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
		routes:         make(map[Intent]model.LLM),
		routeNames:     make(map[Intent]string),
		contextWindows: cloneStringIntMap(cfg.ContextWindows),
		defaultBudget:  cfg.DefaultBudget,
		budgetWarn:     make(map[Intent]struct{}),
		logger:         slog.Default(),
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
		r.defaultModelName = cfg.Model
	}

	for intentName, modelName := range cfg.Routes {
		if modelName == "" {
			continue
		}
		m := NewHugr(querierFor(modelName), modelName, buildOpts()...)
		r.routes[Intent(intentName)] = m
		r.routeNames[Intent(intentName)] = modelName
	}
	return r
}

// cloneStringIntMap returns a defensive copy so mutating the cfg map
// after NewRouter doesn't race with concurrent BudgetFor reads.
func cloneStringIntMap(in map[string]int) map[string]int {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// NewRouterWithDefault builds a Router wrapping a pre-constructed
// default model. Handy for tests + for callers that don't need per-
// model querier routing. Tests that want to exercise BudgetFor on this
// shape can attach budgets after construction via SetBudgets.
func NewRouterWithDefault(defaultModel model.LLM) *Router {
	name := ""
	if defaultModel != nil {
		name = defaultModel.Name()
	}
	return &Router{
		defaultModel:     defaultModel,
		defaultModelName: name,
		routes:           make(map[Intent]model.LLM),
		routeNames:       make(map[Intent]string),
		budgetWarn:       make(map[Intent]struct{}),
		logger:           slog.Default(),
	}
}

// SetBudgets installs context-window budgets after construction. Used
// by tests and by callers that build the router via
// NewRouterWithDefault. Production wiring sets budgets via
// Config.ContextWindows + Config.DefaultBudget at NewRouter time.
//
// Safe to call before the router is shared across goroutines; once the
// router is in concurrent use, treat it as immutable.
func (r *Router) SetBudgets(contextWindows map[string]int, defaultBudget int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.contextWindows = cloneStringIntMap(contextWindows)
	r.defaultBudget = defaultBudget
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
	if r.routeNames == nil {
		r.routeNames = make(map[Intent]string)
	}
	if m != nil {
		r.routeNames[intent] = m.Name()
	}
}

// BudgetFor returns the input-context budget (tokens) of the model
// resolved for `intent`. Resolution chain (spec 006):
//
//  1. ContextWindows[<intent's model name>] when present and > 0
//  2. ContextWindows[<default model name>] when present and > 0 (i.e.
//     the intent has no explicit route — fall back to the default
//     model's window)
//  3. Config.DefaultBudget when > 0
//  4. budgetFloor (with a one-shot INFO log per intent)
//
// Used by pkg/chatcontext.Compactor to compute the rolling-window
// trigger threshold. Safe for concurrent reads under the same
// guarantees as ModelFor.
func (r *Router) BudgetFor(intent Intent) int {
	r.mu.RLock()
	name, ok := r.routeNames[intent]
	if !ok || name == "" {
		name = r.defaultModelName
	}
	cw := r.contextWindows
	defBudget := r.defaultBudget
	r.mu.RUnlock()

	if name != "" {
		if v, ok := cw[name]; ok && v > 0 {
			return v
		}
	}
	if defBudget > 0 {
		return defBudget
	}

	r.budgetWarnMu.Lock()
	_, warned := r.budgetWarn[intent]
	if !warned {
		r.budgetWarn[intent] = struct{}{}
	}
	r.budgetWarnMu.Unlock()
	if !warned && r.logger != nil {
		r.logger.Info(
			"router: no context budget configured for intent — using floor",
			"intent", string(intent),
			"model", name,
			"floor", budgetFloor,
		)
	}
	return budgetFloor
}

// model.LLM interface — delegates via ModelFor(IntentDefault).
func (r *Router) Name() string {
	return r.ModelFor(IntentDefault).Name()
}

func (r *Router) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return r.ModelFor(IntentDefault).GenerateContent(ctx, req, stream)
}
