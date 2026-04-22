package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	hugen "github.com/hugr-lab/hugen/pkg/agent"
	agentstore "github.com/hugr-lab/hugen/pkg/agent/store"
	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/chatcontext"
	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/memory"
	"github.com/hugr-lab/hugen/pkg/models"
	"github.com/hugr-lab/hugen/pkg/models/embedding"
	"github.com/hugr-lab/hugen/pkg/scheduler"
	"github.com/hugr-lab/hugen/pkg/sessions"
	"github.com/hugr-lab/hugen/pkg/skills"
	"github.com/hugr-lab/hugen/pkg/store/local"
	"github.com/hugr-lab/hugen/pkg/tools"
	qe "github.com/hugr-lab/query-engine"
	"github.com/hugr-lab/query-engine/client"
	qetypes "github.com/hugr-lab/query-engine/types"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
)

// agentRuntime bundles all long-lived resources built at startup.
// Shutdown cancels background contexts and closes the engine/client.
type agentRuntime struct {
	agent      agent.Agent
	hugrClient *client.Client
	engine     *qe.Service // nil in hub mode

	sessions *sessions.Manager
	tools    *tools.Manager
	skills   skills.Manager

	classifier *sessions.Classifier
	scheduler  *scheduler.Scheduler
	compactor  *chatcontext.Compactor
	bgCtx      context.Context
	bgCancel   context.CancelFunc
}

// skillsSessionAdapter bridges *sessions.Manager (concrete) to
// skills.SessionAccessor (consumer-defined interface whose Session()
// method returns skills.Session). Go's method-return covariance is
// strict, so we need the tiny wrapper to return the interface.
type skillsSessionAdapter struct{ sm *sessions.Manager }

func (a skillsSessionAdapter) Session(id string) (skills.Session, error) {
	return a.sm.Session(id)
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

// buildComponents bundles the non-hub pieces built during startup:
// hugr client, skills/tools managers, constitution, token estimator.
// The router is built separately because it needs both queriers.
type buildComponents struct {
	hugrClient   *client.Client
	skills       skills.Manager
	tools        *tools.Manager
	constitution string
	tokens       *models.TokenEstimator
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

	constitution, err := os.ReadFile(cfg.Agent.Constitution)
	if err != nil {
		return nil, fmt.Errorf("read constitution %s: %w", cfg.Agent.Constitution, err)
	}

	skillsPath := cfg.Skills.Path
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
		skills:       skillsMgr,
		tools:        toolsMgr,
		constitution: string(constitution),
		tokens:       models.NewTokenEstimator(),
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
			BaseURL:      cfg.A2A.BaseURL,
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

// buildRuntime wires together local engine + hugr client → queriers →
// router / services / session manager → ADK agent. Caller owns
// runtime.close() in the shutdown path.
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

	// Two queriers: local engine (optional) + remote hugr client (always).
	// memoryQuerier is the one hub.db reads/writes go through — prefer
	// local when it's up, otherwise fall back to remote.
	var localQuerier qetypes.Querier
	var remoteQuerier qetypes.Querier = components.hugrClient
	var localModels []string

	if cfg.LocalDBEnabled {
		engine, err := local.New(ctx, cfg.LocalDB, cfg.Identity, cfg.Embedding, logger)
		if err != nil {
			components.hugrClient.CloseSubscriptions()
			return nil, fmt.Errorf("local engine: %w", err)
		}
		rt.engine = engine
		localQuerier = engine
		for _, m := range cfg.LocalDB.Models {
			localModels = append(localModels, m.Name)
		}
	} else {
		logger.Info("hub mode: connecting to", "url", cfg.Hugr.URL)
	}

	memoryQuerier := remoteQuerier
	if localQuerier != nil {
		memoryQuerier = localQuerier
	}

	// Router: gets both queriers + local-model names; builds model.LLMs
	// internally, picking per-model querier by membership in localModels.
	router := models.NewRouter(
		localQuerier,
		remoteQuerier,
		localModels,
		cfg.LLM,
		models.WithLogger(logger),
		models.WithToolChoiceFunc(func() string { return "auto" }),
	).WithLogger(logger)
	for intentName, modelName := range cfg.LLM.Routes {
		logger.Info("intent route configured", "intent", intentName, "model", modelName)
	}

	// Agent-registry client — used for registerAgentInstance in local mode.
	if cfg.LocalDBEnabled {
		reg, err := agentstore.New(memoryQuerier, agentstore.Options{
			AgentID: cfg.Identity.ID, AgentShort: cfg.Identity.ShortID, Logger: logger,
		})
		if err != nil {
			rt.close(logger)
			return nil, fmt.Errorf("agent registry client: %w", err)
		}
		if err := registerAgentInstance(ctx, cfg, reg, logger); err != nil {
			rt.close(logger)
			return nil, err
		}
	}

	// Embeddings adapter — shared by memory service + memory workers.
	embed := embedding.New(memoryQuerier, embedding.Options{
		Model:     cfg.Embedding.Model,
		Dimension: cfg.Embedding.Dimension,
		Logger:    logger,
	})

	// Classifier + scheduler + reviewer + compactor wired before
	// SessionManager so the manager can publish events + queue
	// reviews from the very first Create. All background goroutines
	// start after we're confident buildRuntime won't fail.
	cls := sessions.NewClassifier(memoryQuerier, cfg.Identity.ID, cfg.Identity.ShortID, logger, sessions.DefaultClassifierBuffer)

	loadSkillMemory := func(ctx context.Context, name string) (*skills.SkillMemoryConfig, error) {
		sk, err := components.skills.Load(ctx, name)
		if err != nil || sk == nil {
			return nil, err
		}
		return sk.Memory, nil
	}

	reviewer, err := memory.NewReviewer(memory.ReviewerOptions{
		Querier:         memoryQuerier,
		AgentID:         cfg.Identity.ID,
		AgentShort:      cfg.Identity.ShortID,
		Router:          router,
		Logger:          logger,
		Volatility:      cfg.Memory.VolatilityDuration,
		LoadSkillMemory: loadSkillMemory,
	})
	if err != nil {
		rt.close(logger)
		return nil, fmt.Errorf("build reviewer: %w", err)
	}

	compactor, err := chatcontext.NewCompactor(chatcontext.CompactorOptions{
		Querier:         memoryQuerier,
		AgentID:         cfg.Identity.ID,
		AgentShort:      cfg.Identity.ShortID,
		Router:          router,
		Tokens:          components.tokens,
		Threshold:       cfg.ChatContext.CompactionThreshold,
		Logger:          logger,
		LoadSkillMemory: loadSkillMemory,
	})
	if err != nil {
		rt.close(logger)
		return nil, fmt.Errorf("build compactor: %w", err)
	}

	verifier, err := memory.NewVerifier(memory.VerifierOptions{
		Querier:    memoryQuerier,
		AgentID:    cfg.Identity.ID,
		AgentShort: cfg.Identity.ShortID,
		Router:     router,
		Logger:     logger,
		Volatility: cfg.Memory.VolatilityDuration,
	})
	if err != nil {
		rt.close(logger)
		return nil, fmt.Errorf("build verifier: %w", err)
	}

	consolidator, err := memory.NewConsolidator(memory.ConsolidatorOptions{
		Querier:          memoryQuerier,
		AgentID:          cfg.Identity.ID,
		AgentShort:       cfg.Identity.ShortID,
		HypothesisExpiry: cfg.Memory.Consolidation.HypothesisExpiry,
		Logger:           logger,
	})
	if err != nil {
		rt.close(logger)
		return nil, fmt.Errorf("build consolidator: %w", err)
	}

	sched, err := scheduler.New(scheduler.Runtime{
		Interval:        cfg.Memory.Scheduler.Interval,
		ReviewDelay:     cfg.Memory.Scheduler.ReviewDelay,
		ConsolidationAt: cfg.Memory.Scheduler.ConsolidationAt,
		Reviewer:        reviewer,
		Verifier:        verifier,
		Consolidator:    consolidator,
		Querier:         memoryQuerier,
		AgentID:         cfg.Identity.ID,
		AgentShort:      cfg.Identity.ShortID,
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
	sessionMgr, err := sessions.New(sessions.Config{
		Skills:       components.skills,
		Tools:        components.tools,
		Querier:      memoryQuerier,
		AgentID:      cfg.Identity.ID,
		AgentShort:   cfg.Identity.ShortID,
		Constitution: components.constitution,
		Logger:       logger,
		Classifier:   cls,
		Scheduler:    sched,
		InlineBuilder: func(name, endpoint, authName string, lg *slog.Logger) (tools.Provider, error) {
			return tools.NewMCPProvider(tools.MCPSpec{
				Name:          name,
				Endpoint:      endpoint,
				Auth:          authName,
				AuthStores:    authStores.Tokens,
				BaseTransport: http.DefaultTransport,
				Config:        cfg.MCP,
				Logger:        lg,
			})
		},
	})
	if err != nil {
		rt.close(logger)
		return nil, fmt.Errorf("build session manager: %w", err)
	}
	rt.sessions = sessionMgr

	// Internal services self-register as tools.Providers.
	memService, err := memory.NewService(memoryQuerier, sessionMgr, embed, memory.ServiceOptions{
		AgentID: cfg.Identity.ID, AgentShort: cfg.Identity.ShortID, Logger: logger,
	})
	if err != nil {
		rt.close(logger)
		return nil, fmt.Errorf("build memory service: %w", err)
	}
	chatCtxService, err := chatcontext.NewService(memoryQuerier, sessionMgr, chatcontext.ServiceOptions{
		AgentID: cfg.Identity.ID, AgentShort: cfg.Identity.ShortID, Logger: logger,
	})
	if err != nil {
		rt.close(logger)
		return nil, fmt.Errorf("build chatcontext service: %w", err)
	}
	components.tools.AddProvider(skills.NewService(skillsSessionAdapter{sm: sessionMgr}))
	components.tools.AddProvider(memService)
	components.tools.AddProvider(chatCtxService)
	logger.Info("internal services registered",
		"providers", []string{skills.ServiceName, memory.ServiceName, chatcontext.ServiceName})

	// External providers from config.yaml (MCP only now).
	for _, pc := range cfg.Providers {
		if pc.Type != "mcp" {
			rt.close(logger)
			return nil, fmt.Errorf("provider %q: only type=mcp is supported in config.providers", pc.Name)
		}
		base := http.DefaultTransport
		p, err := tools.NewMCPProvider(tools.MCPSpec{
			Name:          pc.Name,
			Endpoint:      pc.Endpoint,
			Transport:     pc.Transport,
			Command:       pc.Command,
			Args:          pc.Args,
			Env:           pc.Env,
			Auth:          pc.Auth,
			AuthStores:    authStores.Tokens,
			BaseTransport: base,
			Config:        cfg.MCP,
			Logger:        logger,
		})
		if err != nil {
			rt.close(logger)
			return nil, fmt.Errorf("provider %q: %w", pc.Name, err)
		}
		components.tools.AddProvider(p)
		logger.Info("provider registered", "name", pc.Name, "type", pc.Type)
	}

	instruction := memory.WrapInstruction(
		hugen.BaseInstructionProvider(sessionMgr),
		memoryQuerier,
		memory.InjectorOptions{
			AgentID: cfg.Identity.ID, AgentShort: cfg.Identity.ShortID, Logger: logger,
		},
	)

	a, err := hugen.NewAgent(hugen.Runtime{
		Router:               router,
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
func registerAgentInstance(ctx context.Context, cfg *config.Config, reg *agentstore.Client, logger *slog.Logger) error {
	at, err := reg.GetAgentType(ctx, cfg.Identity.Type)
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
	if err := reg.RegisterAgent(ctx, agentstore.Agent{
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
