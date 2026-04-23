package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	hugen "github.com/hugr-lab/hugen/pkg/agent"
	agentstore "github.com/hugr-lab/hugen/pkg/agent/store"
	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/chatcontext"
	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/memory"
	learnstore "github.com/hugr-lab/hugen/pkg/memory/learning/store"
	memstore "github.com/hugr-lab/hugen/pkg/memory/store"
	"github.com/hugr-lab/hugen/pkg/models"
	"github.com/hugr-lab/hugen/pkg/models/embedding"
	"github.com/hugr-lab/hugen/pkg/scheduler"
	"github.com/hugr-lab/hugen/pkg/sessions"
	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
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
	bgCtx      context.Context
	bgCancel   context.CancelFunc
}

func (r *agentRuntime) close(logger *slog.Logger) {
	// Signal every background worker to stop.
	if r.bgCancel != nil {
		r.bgCancel()
	}

	// Scheduler + classifier drain in parallel under a single 5s
	// budget — serial waits could double the shutdown deadline
	// even though the two workers are independent.
	const budget = 5 * time.Second
	deadline := time.Now().Add(budget)
	var wg sync.WaitGroup
	if r.scheduler != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-r.scheduler.Done():
			case <-time.After(time.Until(deadline)):
				logger.Warn("shutting down: scheduler did not stop within budget")
			}
		}()
	}
	if r.classifier != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			drainCtx, cancel := context.WithTimeout(context.Background(), time.Until(deadline))
			defer cancel()
			if err := r.classifier.Drain(drainCtx, time.Until(deadline)); err != nil {
				logger.Warn("shutting down: classifier drain", "err", err)
			}
		}()
	}
	wg.Wait()

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

// runtimeArtifacts bundles the non-hub pieces built during startup:
// hugr client, skills/tools managers, constitution, token estimator.
// The router is built separately because it needs both queriers.
type runtimeArtifacts struct {
	hugrClient   *client.Client
	skills       skills.Manager
	tools        *tools.Manager
	constitution string
	tokens       *models.TokenEstimator
}

// assembleRuntimeArtifacts loads the constitution, constructs the
// skills + tools managers, and packages them alongside the incoming
// hugr client + a token estimator. Called once at the top of
// buildRuntime; separated to keep the pipeline body short.
func assembleRuntimeArtifacts(_ context.Context, cfg *config.Config, hugrClient *client.Client, logger *slog.Logger) (*runtimeArtifacts, error) {
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

	return &runtimeArtifacts{
		hugrClient:   hugrClient,
		skills:       skillsMgr,
		tools:        toolsMgr,
		constitution: string(constitution),
		tokens:       models.NewTokenEstimator(),
	}, nil
}

// buildAuthForBootstrap is Phase A+B.1: builds the single hugr
// Source declared by .env, wires it into a fresh SourceRegistry,
// mounts the shared /auth/callback dispatcher, and returns the
// RoundTripper the hugr client + engine should use.
func buildAuthForBootstrap(ctx context.Context, boot *config.BootstrapConfig, mux *http.ServeMux, logger *slog.Logger) (*auth.SourceRegistry, http.RoundTripper, error) {
	reg := auth.NewSourceRegistry(logger)

	hugrSrc, err := auth.BuildHugrSource(ctx, auth.AuthSpec{
		Name:        boot.HugrAuth.Name,
		Type:        boot.HugrAuth.Type,
		AccessToken: boot.HugrAuth.AccessToken,
		TokenURL:    boot.HugrAuth.TokenURL,
		Issuer:      boot.HugrAuth.Issuer,
		ClientID:    boot.HugrAuth.ClientID,
		BaseURL:     boot.A2A.BaseURL,
		DiscoverURL: boot.Hugr.URL,
	}, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("hugr source: %w", err)
	}
	if err := reg.AddPrimary(hugrSrc); err != nil {
		return nil, nil, err
	}
	if oidc, ok := hugrSrc.(*auth.OIDCStore); ok {
		reg.RegisterPromptLogin(oidc.PromptLogin)
	}
	reg.Mount(mux)

	return reg, auth.Transport(hugrSrc, http.DefaultTransport), nil
}

// loadFullConfig is Phase B.3: chooses between local YAML and
// remote hub pull based on boot.Remote(). In remote mode it also
// resolves agent_id via whoami before the GraphQL fetch.
func loadFullConfig(ctx context.Context, boot *config.BootstrapConfig, hugrClient *client.Client, logger *slog.Logger) (*config.Config, error) {
	if !boot.Remote() {
		return config.LoadLocal("config.yaml", boot)
	}
	who, err := identity.ResolveFromHugr(ctx, hugrClient)
	if err != nil {
		return nil, fmt.Errorf("remote identity: %w", err)
	}
	boot.Identity.ID = who.UserID
	boot.Identity.Name = who.UserName
	logger.Info("remote identity resolved", "agent_id", who.UserID, "name", who.UserName)

	cfg, err := config.LoadRemote(ctx, hugrClient, boot.Identity.ID, boot)
	if err != nil {
		return nil, fmt.Errorf("remote config: %w", err)
	}
	return cfg, nil
}

// buildRuntime wires together local engine + hugr client → queriers →
// router / services / session manager → ADK agent. Caller owns
// runtime.close() in the shutdown path.
func buildRuntime(
	ctx context.Context,
	boot *config.BootstrapConfig,
	cfg *config.Config,
	logger *slog.Logger,
	authReg *auth.SourceRegistry,
	hugrClient *client.Client,
) (*agentRuntime, error) {
	components, err := assembleRuntimeArtifacts(ctx, cfg, hugrClient, logger)
	if err != nil {
		return nil, fmt.Errorf("build components: %w", err)
	}

	rt := &agentRuntime{
		hugrClient: components.hugrClient,
		tools:      components.tools,
		skills:     components.skills,
	}

	// Two Querier endpoints. Remote hugr client is always there; the
	// local embedded engine comes up only when LocalDBEnabled. The
	// "memory" querier is the one hub.db reads/writes route through —
	// prefer local, fall back to remote.
	var (
		localQuerier  qetypes.Querier
		remoteQuerier qetypes.Querier = components.hugrClient
		localModels   []string
	)
	if cfg.LocalDBEnabled {
		engine, models, err := buildLocalHugr(ctx, cfg, logger)
		if err != nil {
			components.hugrClient.CloseSubscriptions()
			return nil, err
		}
		rt.engine = engine
		localQuerier = engine
		localModels = models
	} else {
		logger.Info("hub mode: connecting to", "url", cfg.Hugr.URL)
	}
	memoryQuerier := remoteQuerier
	if localQuerier != nil {
		memoryQuerier = localQuerier
	}

	// Router: picks per-model querier by membership in localModels.
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

	// Self-register runs only in fully local mode — hub owns the
	// agents row in every other combination.
	if !boot.Remote() && cfg.LocalDBEnabled {
		if err := selfRegisterAgent(ctx, cfg, memoryQuerier, logger); err != nil {
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

	// Hub clients — built once and shared across every subsystem
	// (classifier, sessions manager, memory service, reviewer,
	// compactor, consolidator, verifier, injector). Each consumer used
	// to construct its own — five-plus identical instances per
	// process.
	//
	// Every hub here is wired to memoryQuerier. In principle the
	// three slots are independent — e.g. sessions could live on the
	// local engine while memory_items route to a shared remote hub.
	// Today the runtime deliberately collapses all three to one
	// querier; splitting is a future task that needs per-subsystem
	// routing flags in memory.Config + chatcontext.Config + an
	// agent-level sessions routing switch.
	sessHub, err := sessstore.New(memoryQuerier, sessstore.Options{
		AgentID: cfg.Identity.ID, AgentShort: cfg.Identity.ShortID, Logger: logger,
	})
	if err != nil {
		rt.close(logger)
		return nil, fmt.Errorf("build sessions store: %w", err)
	}
	memHub, err := memstore.New(memoryQuerier, memstore.Options{
		AgentID: cfg.Identity.ID, AgentShort: cfg.Identity.ShortID, Logger: logger,
	})
	if err != nil {
		rt.close(logger)
		return nil, fmt.Errorf("build memory store: %w", err)
	}
	learnHub, err := learnstore.New(memoryQuerier, learnstore.Options{
		AgentID: cfg.Identity.ID, AgentShort: cfg.Identity.ShortID, Logger: logger,
	})
	if err != nil {
		rt.close(logger)
		return nil, fmt.Errorf("build learning store: %w", err)
	}

	// Classifier + scheduler + reviewer + compactor wired before
	// SessionManager so the manager can publish events + queue
	// reviews from the very first Create. All background goroutines
	// start after we're confident buildRuntime won't fail.
	cls := sessions.NewClassifierWithHub(sessHub, logger, sessions.DefaultClassifierBuffer)

	loadSkillMemory := func(ctx context.Context, name string) (*skills.SkillMemoryConfig, error) {
		sk, err := components.skills.Load(ctx, name)
		if err != nil || sk == nil {
			return nil, err
		}
		return sk.Memory, nil
	}

	mem, err := memory.New(memory.Options{
		Querier: memoryQuerier,
		AgentID: cfg.Identity.ID, AgentShort: cfg.Identity.ShortID,
		Logger:          logger,
		Memory:          memHub,
		Learning:        learnHub,
		Sessions:        sessHub,
		Router:          router,
		Tokens:          components.tokens,
		Config:          cfg.Memory,
		LoadSkillMemory: loadSkillMemory,
	})
	if err != nil {
		rt.close(logger)
		return nil, fmt.Errorf("build memory: %w", err)
	}

	chat, err := chatcontext.New(chatcontext.Options{
		Querier: memoryQuerier,
		AgentID: cfg.Identity.ID, AgentShort: cfg.Identity.ShortID,
		Logger:   logger,
		Memory:   memHub,
		Sessions: sessHub,
		Router:   router,
		Tokens:   components.tokens,
		// Spec 006 §1: coordinator's compactor sizes against the
		// strong-model window (router resolves IntentDefault to
		// cfg.LLM.Model). Sub-agent dispatch builds its own per-
		// mission compactor with the role's intent so a cheap-model
		// specialist compacts at the cheap-model window.
		Intent:          models.IntentDefault,
		Threshold:       cfg.ChatContext.CompactionThreshold,
		LoadSkillMemory: loadSkillMemory,
	})
	if err != nil {
		rt.close(logger)
		return nil, fmt.Errorf("build chatcontext: %w", err)
	}

	sched := scheduler.New(logger)
	if err := mem.RegisterTasks(sched); err != nil {
		rt.close(logger)
		return nil, fmt.Errorf("register memory tasks: %w", err)
	}
	rt.classifier = cls
	rt.scheduler = sched

	// SessionManager wires classifier + scheduler in so IngestADKEvent
	// publishes transcript rows and Delete queues post-session review.
	sessionMgr, err := sessions.New(sessions.Config{
		Skills:       components.skills,
		Tools:        components.tools,
		Sessions:     sessHub,
		AgentID:      cfg.Identity.ID,
		AgentShort:   cfg.Identity.ShortID,
		Constitution: components.constitution,
		Logger:       logger,
		Classifier:   cls,
		Scheduler:    mem.Reviewer(),
		InlineBuilder: func(name, endpoint string, a sessions.InlineProviderAuth, lg *slog.Logger) (tools.Provider, error) {
			return tools.NewMCPProvider(tools.MCPSpec{
				Name:            name,
				Endpoint:        endpoint,
				Auth:            a.Name,
				AuthType:        a.Type,
				AuthHeaderName:  a.HeaderName,
				AuthHeaderValue: a.HeaderValue,
				AuthStores:      authReg.TokenStores(),
				BaseTransport:   http.DefaultTransport,
				Config:          cfg.MCP,
				Logger:          lg,
			})
		},
	})
	if err != nil {
		rt.close(logger)
		return nil, fmt.Errorf("build session manager: %w", err)
	}
	rt.sessions = sessionMgr

	// SessionManager is up — ChatContext can now own its Service
	// (which needs sessions.Manager for the intro tool) and we can
	// build the memory tools provider the same way.
	if err := chat.AttachSessions(sessionMgr); err != nil {
		rt.close(logger)
		return nil, fmt.Errorf("attach chatcontext service: %w", err)
	}
	memService, err := memory.NewService(memoryQuerier, sessionMgr, embed, memory.ServiceOptions{
		AgentID: cfg.Identity.ID, AgentShort: cfg.Identity.ShortID, Logger: logger,
		Memory: memHub, Sessions: sessHub,
	})
	if err != nil {
		rt.close(logger)
		return nil, fmt.Errorf("build memory service: %w", err)
	}
	components.tools.AddProvider(skills.NewService(sessionMgr.SkillsAccessor()))
	components.tools.AddProvider(memService)
	components.tools.AddProvider(chat.Provider())
	logger.Info("internal services registered",
		"providers", []string{skills.ServiceName, memory.ServiceName, chatcontext.ServiceName})

	// External providers from config.yaml. Internal services (type=system)
	// are registered programmatically above — skip their YAML
	// declarations to keep back-compat with existing configs.
	for _, pc := range cfg.Providers {
		if pc.Type == "system" {
			continue
		}
		if pc.Type != "mcp" {
			rt.close(logger)
			return nil, fmt.Errorf("provider %q: only type=mcp is supported in config.providers", pc.Name)
		}
		base := http.DefaultTransport
		p, err := tools.NewMCPProvider(tools.MCPSpec{
			Name:            pc.Name,
			Endpoint:        pc.Endpoint,
			Transport:       pc.Transport,
			Command:         pc.Command,
			Args:            pc.Args,
			Env:             pc.Env,
			Auth:            pc.Auth,
			AuthType:        pc.AuthType,
			AuthHeaderName:  pc.AuthHeaderName,
			AuthHeaderValue: pc.AuthHeaderValue,
			AuthStores:      authReg.TokenStores(),
			BaseTransport:   base,
			Config:          cfg.MCP,
			Logger:          logger,
		})
		if err != nil {
			rt.close(logger)
			return nil, fmt.Errorf("provider %q: %w", pc.Name, err)
		}
		components.tools.AddProvider(p)
		logger.Info("provider registered", "name", pc.Name, "type", pc.Type)
	}

	instruction := mem.InstructionProvider(hugen.BaseInstructionProvider(sessionMgr))

	a, err := hugen.NewAgent(hugen.Runtime{
		Router:               router,
		Sessions:             sessionMgr,
		Tokens:               components.tokens,
		ExtraBeforeCallbacks: []llmagent.BeforeModelCallback{chat.Callback()},
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

	// Start background workers. bgCtx inherits from the caller's
	// signal-aware ctx so SIGINT flows straight through to the
	// classifier / scheduler / session-cleanup loops without any
	// defer-chain indirection.
	rt.bgCtx, rt.bgCancel = context.WithCancel(ctx)
	go cls.Run(rt.bgCtx)
	sched.Start(rt.bgCtx)
	hugen.StartSessionCleanup(rt.bgCtx, sessionMgr, 1*time.Hour, logger)

	return rt, nil
}

// buildLocalHugr brings up the embedded query-engine backed by
// cfg.LocalDB and returns it along with the model names the router
// should route to it. Called only when cfg.LocalDBEnabled.
func buildLocalHugr(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*qe.Service, []string, error) {
	engine, err := local.New(ctx, cfg.LocalDB, cfg.Identity, cfg.Embedding, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("local engine: %w", err)
	}
	models := make([]string, 0, len(cfg.LocalDB.Models))
	for _, m := range cfg.LocalDB.Models {
		models = append(models, m.Name)
	}
	return engine, models, nil
}

// selfRegisterAgent constructs a short-lived agentstore client and
// upserts the agents row. Runs only in fully-local mode — in every
// other combination the hub owns the registry entry.
func selfRegisterAgent(ctx context.Context, cfg *config.Config, querier qetypes.Querier, logger *slog.Logger) error {
	reg, err := agentstore.New(querier, agentstore.Options{
		AgentID: cfg.Identity.ID, AgentShort: cfg.Identity.ShortID, Logger: logger,
	})
	if err != nil {
		return fmt.Errorf("agent registry client: %w", err)
	}
	return registerAgentInstance(ctx, cfg, reg, logger)
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
