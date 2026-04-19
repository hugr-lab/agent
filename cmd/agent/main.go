// Package main is the entry point for the hugr-agent runtime.
package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/gorilla/mux"
	"github.com/hugr-lab/hugen/adapters/file"
	"github.com/hugr-lab/hugen/interfaces"
	"github.com/hugr-lab/hugen/internal/config"
	hugen "github.com/hugr-lab/hugen/pkg/agent"
	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/llms/intent"
	"github.com/hugr-lab/hugen/pkg/models/hugr"
	"github.com/hugr-lab/hugen/pkg/providers"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/skills"
	"github.com/hugr-lab/hugen/pkg/tools"
	qe "github.com/hugr-lab/query-engine"
	"github.com/hugr-lab/query-engine/client"
	qetypes "github.com/hugr-lab/query-engine/types"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/artifact"
	"google.golang.org/adk/cmd/launcher"
	"google.golang.org/adk/cmd/launcher/full"
	"google.golang.org/adk/cmd/launcher/web/webui"
	"google.golang.org/adk/memory"
	"google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/server/adka2a"
	"google.golang.org/adk/server/adkrest"
	adksession "google.golang.org/adk/session"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	logLevel := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		logLevel = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel,
	}))

	logger.Info("hugr-agent starting",
		"hugr_url", cfg.Hugr.URL,
		"model", cfg.Agent.Model,
	)

	mode := ""
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}

	switch mode {
	case "console":
		consoleMux := http.NewServeMux()
		authStores, err := buildAuthStores(ctx, cfg, logger, consoleMux)
		if err != nil {
			log.Fatalf("auth: %v", err)
		}
		hugrTransport := resolveHugrTransport(cfg, authStores, logger)

		addr := fmt.Sprintf(":%d", cfg.Agent.Port)
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			log.Fatalf("listen %s: %v", addr, err)
		}
		go func() {
			if err := http.Serve(listener, consoleMux); err != nil {
				logger.Error("callback server error", "err", err)
			}
		}()
		for _, p := range authStores.PromptLogin {
			go p()
		}

		rt, err := buildRuntime(ctx, cfg, logger, authStores, hugrTransport)
		if err != nil {
			log.Fatalf("Failed to build runtime: %v", err)
		}
		defer rt.close(logger)
		l := full.NewLauncher()
		if err := l.Execute(ctx, &launcher.Config{
			AgentLoader:    agent.NewSingleLoader(rt.agent),
			SessionService: rt.sessions,
		}, os.Args[2:]); err != nil {
			log.Fatalf("Launcher error: %v\n\n%s", err, l.CommandLineSyntax())
		}
	case "devui":
		if err := runWithDevUI(ctx, cfg, logger); err != nil && ctx.Err() == nil {
			log.Fatalf("Server error: %v", err)
		}
	default:
		if err := runA2A(ctx, cfg, logger); err != nil && ctx.Err() == nil {
			log.Fatalf("Server error: %v", err)
		}
	}

	logger.Info("shutdown complete")
}

// buildComponents bundles the non-hub pieces built during startup: the
// hugr LLM client, router, skills/tools managers, constitution.
type buildComponents struct {
	hugrClient   *client.Client
	router       *intent.Router
	skills       skills.Manager
	tools        *tools.Manager
	constitution string
	tokens       *hugen.TokenEstimator
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
		tokens:       hugen.NewTokenEstimator(),
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

// a2aHandlers returns the agent card handler and JSON-RPC invoke handler.
func a2aHandlers(a agent.Agent, sessionSvc adksession.Service, artifactSvc artifact.Service, baseURL string) (cardHandler, invokeHandler http.Handler) {
	agentCard := &a2a.AgentCard{
		Name:               a.Name(),
		Description:        a.Description(),
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		URL:                baseURL + "/invoke",
		PreferredTransport: a2a.TransportProtocolJSONRPC,
		Skills:             adka2a.BuildAgentSkills(a),
		Capabilities:       a2a.AgentCapabilities{Streaming: true},
	}
	executor := adka2a.NewExecutor(adka2a.ExecutorConfig{
		RunnerConfig: runner.Config{
			AppName:         a.Name(),
			Agent:           a,
			SessionService:  sessionSvc,
			ArtifactService: artifactSvc,
		},
	})
	return a2asrv.NewStaticAgentCardHandler(agentCard), a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(executor))
}

// agentRuntime bundles all long-lived resources built at startup. Shutdown
// closes them in the correct order: server → HubDB → engine → hugrClient.
type agentRuntime struct {
	agent      agent.Agent
	hugrClient *client.Client
	engine     *qe.Service // nil in hub mode
	hubDB      interfaces.HubDB
	sessions   interfaces.SessionManager
	tools      *tools.Manager
	skills     skills.Manager
}

func (r *agentRuntime) close(logger *slog.Logger) {
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

	// SessionManager first (without a concrete Skills-suite provider in
	// tools yet — but InlineBuilder for skill endpoints works already).
	// We build the provider registry *after* SessionManager exists
	// because the system suite needs SessionManager as a dep.
	sessionMgr := session.New(session.Config{
		Skills:       components.skills,
		Tools:        components.tools,
		Hub:          hub,
		Constitution: components.constitution,
		Logger:       logger,
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
		MCP:           cfg.MCP,
		Logger:        logger,
	}); err != nil {
		rt.close(logger)
		return nil, err
	}

	a, err := hugen.NewAgent(hugen.Config{
		Router:   components.router,
		Sessions: sessionMgr,
		Tokens:   components.tokens,
		Logger:   logger,
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

	return rt, nil
}

// registerAgentInstance verifies the agent_type row exists (seeded at
// migration) and upserts the agents row with the current config_override.
// Runs only in local mode — in hub mode the hub owns registration.
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

// serveAndShutdown starts the HTTP server and handles graceful shutdown.
// postListen functions run once after the listener is bound (used to
// prompt OIDC login only after /auth/<name>/callback is actually
// reachable) — one entry per configured OIDC auth.
func serveAndShutdown(ctx context.Context, srv *http.Server, rt *agentRuntime, postListen []func(), logger *slog.Logger) error {
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		logger.Info("shutting down: draining requests")
		srv.Shutdown(shutdownCtx)
		rt.close(logger)
	}()
	listener, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return err
	}
	for _, p := range postListen {
		go p()
	}
	return srv.Serve(listener)
}

// runA2A starts the A2A server (default mode).
func runA2A(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	artifactSvc := artifact.InMemoryService()

	smux := http.NewServeMux()
	authStores, err := buildAuthStores(ctx, cfg, logger, smux)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	hugrTransport := resolveHugrTransport(cfg, authStores, logger)

	rt, err := buildRuntime(ctx, cfg, logger, authStores, hugrTransport)
	if err != nil {
		return err
	}

	cardH, invokeH := a2aHandlers(rt.agent, rt.sessions, artifactSvc, cfg.Agent.BaseURL)
	smux.Handle(a2asrv.WellKnownAgentCardPath, cardH)
	smux.Handle("/invoke", invokeH)
	registerAdminRoutes(smux, rt.tools, rt.skills)

	srv := &http.Server{Addr: fmt.Sprintf(":%d", cfg.Agent.Port), Handler: smux}
	logger.Info("A2A server listening",
		"addr", srv.Addr,
		"invoke", cfg.Agent.BaseURL+"/invoke",
	)
	return serveAndShutdown(ctx, srv, rt, authStores.PromptLogin, logger)
}

// runWithDevUI starts A2A + ADK REST API + dev UI.
func runWithDevUI(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	artifactSvc := artifact.InMemoryService()

	router := mux.NewRouter()

	// OIDC callback routes on a separate ServeMux (gorilla/mux can't use http.ServeMux patterns).
	authMux := http.NewServeMux()
	authStores, err := buildAuthStores(ctx, cfg, logger, authMux)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	hugrTransport := resolveHugrTransport(cfg, authStores, logger)
	router.PathPrefix("/auth/").Handler(authMux)

	rt, err := buildRuntime(ctx, cfg, logger, authStores, hugrTransport)
	if err != nil {
		return err
	}

	// A2A.
	cardH, invokeH := a2aHandlers(rt.agent, rt.sessions, artifactSvc, cfg.Agent.BaseURL)
	router.Handle(a2asrv.WellKnownAgentCardPath, cardH)
	router.Handle("/invoke", invokeH)
	registerAdminRoutes(muxShim{router}, rt.tools, rt.skills)

	// ADK REST API + dev UI.
	agentLoader := agent.NewSingleLoader(rt.agent)
	memorySvc := memory.InMemoryService()
	apiServer, err := adkrest.NewServer(adkrest.ServerConfig{
		SessionService:  rt.sessions,
		ArtifactService: artifactSvc,
		MemoryService:   memorySvc,
		AgentLoader:     agentLoader,
		SSEWriteTimeout: 120 * time.Second,
		DebugConfig:     &adkrest.DebugTelemetryConfig{},
	})
	if err != nil {
		return fmt.Errorf("create REST server: %w", err)
	}
	router.PathPrefix("/api").Handler(
		corsMiddleware(cfg.Agent.BaseURL, http.StripPrefix("/api", apiServer)),
	)

	ui := webui.NewLauncher()
	apiAddr := cfg.Agent.BaseURL + "/api"
	if _, err := ui.Parse([]string{"-api_server_address", apiAddr}); err != nil {
		return fmt.Errorf("parse webui flags: %w", err)
	}
	if err := ui.SetupSubrouters(router, &launcher.Config{
		SessionService:  rt.sessions,
		ArtifactService: artifactSvc,
		AgentLoader:     agentLoader,
	}); err != nil {
		return fmt.Errorf("setup webui: %w", err)
	}

	srv := &http.Server{Addr: fmt.Sprintf(":%d", cfg.Agent.Port), Handler: router}
	logger.Info("A2A + dev UI server listening",
		"addr", srv.Addr,
		"invoke", cfg.Agent.BaseURL+"/invoke",
		"ui", cfg.Agent.BaseURL+"/",
	)
	return serveAndShutdown(ctx, srv, rt, authStores.PromptLogin, logger)
}

func corsMiddleware(baseURL string, next http.Handler) http.Handler {
	// Extract origin from baseURL (e.g. "http://localhost:10000" → same).
	// Allow "*" only for localhost; restrict to baseURL origin otherwise.
	origin := baseURL
	if strings.Contains(baseURL, "localhost") || strings.Contains(baseURL, "127.0.0.1") {
		origin = "*"
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

