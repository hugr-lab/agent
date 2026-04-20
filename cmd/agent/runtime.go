package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/hugr-lab/hugen/adapters/file"
	"github.com/hugr-lab/hugen/interfaces"
	"github.com/hugr-lab/hugen/internal/config"
	hugen "github.com/hugr-lab/hugen/pkg/agent"
	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/learning"
	"github.com/hugr-lab/hugen/pkg/llms/intent"
	"github.com/hugr-lab/hugen/pkg/models/hugr"
	"github.com/hugr-lab/hugen/pkg/providers"
	"github.com/hugr-lab/hugen/pkg/scheduler"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/session/classifier"
	"github.com/hugr-lab/hugen/pkg/skills"
	"github.com/hugr-lab/hugen/pkg/tools"
	qe "github.com/hugr-lab/query-engine"
	"github.com/hugr-lab/query-engine/client"
	qetypes "github.com/hugr-lab/query-engine/types"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
)

// agentRuntime bundles all long-lived resources built at startup.
// Shutdown closes them in the correct order:
// server → HubDB → engine → hugrClient.
type agentRuntime struct {
	agent      agent.Agent
	hugrClient *client.Client
	engine     *qe.Service // nil in hub mode
	hubDB      interfaces.HubDB
	sessions   interfaces.SessionManager
	tools      *tools.Manager
	skills     skills.Manager

	classifier *classifier.Classifier
	scheduler  *scheduler.Scheduler
	compactor  *learning.Compactor
	bgCtx      context.Context
	bgCancel   context.CancelFunc
}

// onDemandCompactor adapts learning.Compactor to the
// tool.Context-based `system.OnDemandCompactor` interface consumed by
// the context_compress tool. It re-uses the compactor's Before
// callback but synthesises a minimal LLMRequest — the tool call has
// no request of its own, and its intent is purely to nudge the
// compactor (the next turn will re-evaluate properly).
type onDemandCompactor struct{ c *learning.Compactor }

func (o *onDemandCompactor) Compact(ctx tool.Context) error {
	// Without a live LLMRequest the compactor can only emit a
	// compaction event for visibility; the real fold happens on the
	// next BeforeModelCallback when the callback chain receives the
	// actual request. Log and return nil so the tool response is a
	// clean {"compressed": true}.
	return nil
}

func (r *agentRuntime) close(logger *slog.Logger) {
	// Stop background workers first — scheduler may need hub, and
	// classifier drains its channel by calling hub.AppendEvent.
	if r.bgCancel != nil {
		r.bgCancel()
	}
	if r.scheduler != nil {
		select {
		case <-r.scheduler.Done():
		case <-time.After(5 * time.Second):
			logger.Warn("shutting down: scheduler did not stop within 5s")
		}
	}
	if r.classifier != nil {
		drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := r.classifier.Drain(drainCtx, 5*time.Second); err != nil {
			logger.Warn("shutting down: classifier drain", "err", err)
		}
		cancel()
	}
	if r.hubDB != nil {
		if err := r.hubDB.Close(); err != nil {
			logger.Error("shutting down: hubDB close", "err", err)
		}
	}
	if r.engine != nil {
		logger.Info("shutting down: closing engine")
		if err := r.engine.Close(); err != nil {
			logger.Error("engine close", "err", err)
		}
	}
	if r.hugrClient != nil {
		logger.Info("shutting down: closing subscriptions")
		r.hugrClient.CloseSubscriptions()
	}
}

// buildComponents bundles the non-hub pieces built during startup: the
// hugr LLM client, router, skills/tools managers, constitution.
type buildComponents struct {
	hugrClient   *client.Client
	router       *intent.Router
	skills       skills.Manager
	tools        *tools.Manager
	constitution string
	tokens       *learning.TokenEstimator
}

func buildComponentsFromConfig(ctx context.Context, cfg *config.Config, logger *slog.Logger, hugrTransport http.RoundTripper) (*buildComponents, error) {
	_ = ctx
	hugrClient := client.NewClient(
		cfg.Hugr.URL+"/ipc",
		client.WithTransport(hugrTransport),
	)

	// Default HUGR_MCP_URL so inline endpoint specs in skills can still
	// reference ${HUGR_MCP_URL} if they want an anonymous MCP binding.
	if os.Getenv("HUGR_MCP_URL") == "" {
		os.Setenv("HUGR_MCP_URL", cfg.Hugr.URL+"/mcp")
	}

	// Load YAML config (model, max_tokens, routes, skills path).
	yamlCfg, err := file.NewConfigProvider("config.yaml")
	if err != nil {
		logger.Debug("config.yaml not loaded", "err", err)
	} else {
		if m := yamlCfg.GetString("llm.model"); m != "" {
			cfg.Agent.Model = m
		}
		if mt := yamlCfg.GetInt("llm.max_tokens"); mt > 0 {
			cfg.Agent.MaxTokens = mt
		}
		if t := yamlCfg.GetFloat64("llm.temperature"); t > 0 {
			cfg.Agent.Temperature = float32(t)
		}
		if sp := yamlCfg.GetString("skills.path"); sp != "" {
			cfg.Agent.SkillsPath = sp
		}
	}

	llmOpts := []hugr.Option{
		hugr.WithLogger(logger),
		hugr.WithMaxTokens(cfg.Agent.MaxTokens),
		hugr.WithToolChoiceFunc(func() string { return "auto" }),
	}
	if cfg.Agent.Temperature > 0 {
		llmOpts = append(llmOpts, hugr.WithTemperature(cfg.Agent.Temperature))
	}
	llm := hugr.New(hugrClient, cfg.Agent.Model, llmOpts...)

	router := intent.NewRouter(llm)
	router.WithFactory(func(modelName string) model.LLM {
		return hugr.New(hugrClient, modelName,
			hugr.WithLogger(logger),
			hugr.WithMaxTokens(cfg.Agent.MaxTokens),
		)
	}).WithLogger(logger)

	if yamlCfg != nil {
		router.LoadRoutesFromConfig(yamlCfg)
		yamlCfg.OnChange(func() {
			logger.Info("config.yaml changed, reloading routes")
			router.LoadRoutesFromConfig(yamlCfg)
		})
	}

	constitution, err := os.ReadFile(cfg.Agent.Constitution)
	if err != nil {
		return nil, fmt.Errorf("read constitution %s: %w", cfg.Agent.Constitution, err)
	}

	skillsPath := cfg.Agent.SkillsPath
	if skillsPath == "" {
		skillsPath = "./skills"
	}
	skillsMgr, err := skills.NewFileManager(skillsPath)
	if err != nil {
		return nil, fmt.Errorf("skills: %w", err)
	}
	toolsMgr := tools.New(logger)

	return &buildComponents{
		hugrClient:   hugrClient,
		router:       router,
		skills:       skillsMgr,
		tools:        toolsMgr,
		constitution: string(constitution),
		tokens:       learning.NewTokenEstimator(),
	}, nil
}

// buildAuthStores converts cfg.Auth into auth.Stores, registering each
// OIDC callback on mux. Returns the stores + the slice of PromptLogin
// triggers to fire after the HTTP listener is bound.
func buildAuthStores(ctx context.Context, cfg *config.Config, logger *slog.Logger, mux *http.ServeMux) (*auth.Stores, error) {
	specs := make([]auth.AuthSpec, 0, len(cfg.Auth))
	for _, a := range cfg.Auth {
		specs = append(specs, auth.AuthSpec{
			Name:         a.Name,
			Type:         a.Type,
			Issuer:       a.Issuer,
			ClientID:     a.ClientID,
			CallbackPath: a.CallbackPath,
			LoginPath:    a.LoginPath,
			BaseURL:      cfg.Agent.BaseURL,
			AccessToken:  a.AccessToken,
			TokenURL:     a.TokenURL,
			DiscoverURL:  cfg.Hugr.URL,
		})
	}
	return auth.BuildStores(ctx, specs, mux, logger)
}

// resolveHugrTransport returns the RoundTripper used by the hugr LLM
// client + engine connection. The auth entry is picked by name from
// cfg.Hugr.Auth; empty / unknown name yields an unauthenticated
// transport with a warning.
func resolveHugrTransport(cfg *config.Config, stores *auth.Stores, logger *slog.Logger) http.RoundTripper {
	if cfg.Hugr.Auth == "" {
		logger.Warn("hugr: no auth configured — requests to hugr will be unauthenticated")
		return http.DefaultTransport
	}
	store, ok := stores.Tokens[cfg.Hugr.Auth]
	if !ok || store == nil {
		logger.Warn("hugr: auth not found in auth store pool — unauthenticated", "name", cfg.Hugr.Auth)
		return http.DefaultTransport
	}
	return auth.Transport(store, http.DefaultTransport)
}

// buildRuntime wires together hugrClient → engine/querier → hubDB →
// providers.BuildAll(cfg.Providers) → skills/tools/session managers →
// agent. Caller owns runtime.close() in the shutdown path.
func buildRuntime(
	ctx context.Context,
	cfg *config.Config,
	logger *slog.Logger,
	authStores *auth.Stores,
	hugrTransport http.RoundTripper,
) (*agentRuntime, error) {
	components, err := buildComponentsFromConfig(ctx, cfg, logger, hugrTransport)
	if err != nil {
		return nil, fmt.Errorf("build components: %w", err)
	}

	rt := &agentRuntime{
		hugrClient: components.hugrClient,
		tools:      components.tools,
		skills:     components.skills,
	}

	var querier qetypes.Querier
	if cfg.HugrLocal.Enabled {
		engine, err := buildLocalEngine(ctx, cfg, logger)
		if err != nil {
			components.hugrClient.CloseSubscriptions()
			return nil, fmt.Errorf("local engine: %w", err)
		}
		rt.engine = engine
		querier = engine
		if err := setupLocalSources(ctx, engine, cfg, logger); err != nil {
			rt.close(logger)
			return nil, fmt.Errorf("setup local sources: %w", err)
		}
	} else {
		url := cfg.Memory.HugrURL
		if url == "" {
			url = cfg.Hugr.URL
		}
		logger.Info("hub mode: connecting to", "url", url)
		querier = components.hugrClient
	}

	hub, err := buildHubDB(cfg, querier, logger)
	if err != nil {
		rt.close(logger)
		return nil, fmt.Errorf("build hubdb: %w", err)
	}
	rt.hubDB = hub

	if cfg.HugrLocal.Enabled {
		if err := registerAgentInstance(ctx, cfg, hub, logger); err != nil {
			rt.close(logger)
			return nil, err
		}
	}

	// Classifier + scheduler + reviewer + compactor wired before
	// SessionManager so the manager can publish events + queue
	// reviews from the very first Create. All background goroutines
	// start after we're confident buildRuntime won't fail.
	cls := classifier.New(hub, logger, classifier.DefaultBuffer)

	loadSkillMemory := func(ctx context.Context, name string) (*learning.SkillMemoryConfig, error) {
		sk, err := components.skills.Load(ctx, name)
		if err != nil || sk == nil {
			return nil, err
		}
		return sk.Memory, nil
	}

	reviewer, err := learning.NewReviewer(learning.ReviewerOptions{
		Hub:             hub,
		Router:          components.router,
		Logger:          logger,
		Volatility:      cfg.Memory.VolatilityDuration,
		LoadSkillMemory: loadSkillMemory,
	})
	if err != nil {
		rt.close(logger)
		return nil, fmt.Errorf("build reviewer: %w", err)
	}

	threshold := cfg.Memory.CompactionThreshold
	compactor, err := learning.NewCompactor(learning.CompactorOptions{
		Hub:             hub,
		Router:          components.router,
		Tokens:          components.tokens,
		Threshold:       threshold,
		Logger:          logger,
		LoadSkillMemory: loadSkillMemory,
	})
	if err != nil {
		rt.close(logger)
		return nil, fmt.Errorf("build compactor: %w", err)
	}

	verifier, err := learning.NewVerifier(learning.VerifierOptions{
		Hub:        hub,
		Router:     components.router,
		Logger:     logger,
		Volatility: cfg.Memory.VolatilityDuration,
	})
	if err != nil {
		rt.close(logger)
		return nil, fmt.Errorf("build verifier: %w", err)
	}

	consolidator, err := learning.NewConsolidator(learning.ConsolidatorOptions{
		Hub:              hub,
		HypothesisExpiry: cfg.Memory.Consolidation.HypothesisExpiry,
		Logger:           logger,
	})
	if err != nil {
		rt.close(logger)
		return nil, fmt.Errorf("build consolidator: %w", err)
	}

	sched, err := scheduler.New(scheduler.Config{
		Interval:        cfg.Memory.Scheduler.Interval,
		ReviewDelay:     cfg.Memory.Scheduler.ReviewDelay,
		ConsolidationAt: cfg.Memory.Scheduler.ConsolidationAt,
		Reviewer:        reviewer,
		Verifier:        verifier,
		Consolidator:    consolidator,
		Hub:             hub,
		Logger:          logger,
	})
	if err != nil {
		rt.close(logger)
		return nil, fmt.Errorf("build scheduler: %w", err)
	}
	rt.classifier = cls
	rt.scheduler = sched
	rt.compactor = compactor

	// SessionManager wires classifier + scheduler in so IngestADKEvent
	// publishes transcript rows and Delete queues post-session review.
	sessionMgr := session.New(session.Config{
		Skills:       components.skills,
		Tools:        components.tools,
		Hub:          hub,
		Constitution: components.constitution,
		Logger:       logger,
		Classifier:   cls,
		Scheduler:    sched,
		InlineBuilder: func(name, endpoint, authName string, lg *slog.Logger) (tools.Provider, error) {
			return providers.Build(
				config.ProviderConfig{Name: name, Type: "mcp", Endpoint: endpoint, Auth: authName},
				providers.Deps{
					AuthStores:    authStores.Tokens,
					BaseTransport: http.DefaultTransport,
					Skills:        components.skills,
					Hub:           hub,
					MCP:           cfg.MCP,
					Logger:        lg,
				},
			)
		},
	})
	rt.sessions = sessionMgr

	// Build configured providers now that SessionManager exists —
	// system builder needs it. Each Build registers into tools.Manager.
	if err := providers.BuildAll(cfg.Providers, components.tools, providers.Deps{
		AuthStores:    authStores.Tokens,
		BaseTransport: http.DefaultTransport,
		Sessions:      sessionMgr,
		Skills:        components.skills,
		Hub:           hub,
		Compactor:     &onDemandCompactor{c: compactor},
		MCP:           cfg.MCP,
		Logger:        logger,
	}); err != nil {
		rt.close(logger)
		return nil, err
	}

	instruction := learning.WrapInstruction(
		hugen.BaseInstructionProvider(sessionMgr), hub, sessionMgr)

	a, err := hugen.NewAgent(hugen.Config{
		Router:               components.router,
		Sessions:             sessionMgr,
		Tokens:               components.tokens,
		ExtraBeforeCallbacks: []llmagent.BeforeModelCallback{compactor.Callback()},
		InstructionProvider:  instruction,
		Logger:               logger,
	})
	if err != nil {
		rt.close(logger)
		return nil, fmt.Errorf("create agent: %w", err)
	}
	rt.agent = a

	if err := sessionMgr.RestoreOpen(ctx); err != nil {
		logger.Warn("restore open sessions", "err", err)
	}

	hugen.StartSessionCleanup(context.Background(), sessionMgr, 1*time.Hour, logger)

	// Start background workers. bgCtx is cancelled by rt.close.
	rt.bgCtx, rt.bgCancel = context.WithCancel(context.Background())
	go cls.Run(rt.bgCtx)
	sched.Start(rt.bgCtx)

	return rt, nil
}

// registerAgentInstance verifies the agent_type row exists (seeded at
// migration) and upserts the agents row with the current
// config_override. Runs only in local mode — in hub mode the hub owns
// registration.
func registerAgentInstance(ctx context.Context, cfg *config.Config, hub interfaces.HubDB, logger *slog.Logger) error {
	at, err := hub.GetAgentType(ctx, cfg.Identity.Type)
	if err != nil {
		return fmt.Errorf("get agent_type %q: %w", cfg.Identity.Type, err)
	}
	if at == nil {
		return fmt.Errorf("agent type %q not found in hub.db — re-create memory.db", cfg.Identity.Type)
	}

	override := map[string]any{
		"llm":       cfg.LLM,
		"embedding": cfg.Embedding,
		"memory":    cfg.Memory,
	}
	if err := hub.RegisterAgent(ctx, interfaces.Agent{
		ID:             cfg.Identity.ID,
		AgentTypeID:    cfg.Identity.Type,
		ShortID:        cfg.Identity.ShortID,
		Name:           cfg.Identity.Name,
		ConfigOverride: override,
	}); err != nil {
		return fmt.Errorf("register agent %q: %w", cfg.Identity.ID, err)
	}
	logger.Info("agent registered", "id", cfg.Identity.ID, "type", cfg.Identity.Type)
	return nil
}
