// Package main is the entry point for the hugr-agent runtime.
package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/gorilla/mux"
	"github.com/hugr-lab/agent/adapters/file"
	"github.com/hugr-lab/agent/internal/config"
	"github.com/hugr-lab/agent/pkg/auth"
	"github.com/hugr-lab/agent/pkg/hugragent"
	"github.com/hugr-lab/agent/pkg/hugrmodel"
	"github.com/hugr-lab/agent/pkg/intentllm"
	"github.com/hugr-lab/agent/pkg/systemtools"
	"github.com/hugr-lab/query-engine/client"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/artifact"
	"google.golang.org/adk/model"
	"google.golang.org/adk/cmd/launcher"
	"google.golang.org/adk/cmd/launcher/full"
	"google.golang.org/adk/cmd/launcher/web/webui"
	"google.golang.org/adk/memory"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/server/adka2a"
	"google.golang.org/adk/server/adkrest"
	"google.golang.org/adk/session"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
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
		hugrTransport := buildHugrTransport(cfg, logger, consoleMux)

		go func() {
			addr := fmt.Sprintf(":%d", cfg.Agent.Port)
			if err := http.ListenAndServe(addr, consoleMux); err != nil {
				logger.Error("callback server error", "err", err)
			}
		}()

		a, hugrClient, err := buildAgent(cfg, logger, hugrTransport)
		if err != nil {
			log.Fatalf("Failed to build agent: %v", err)
		}
		defer hugrClient.CloseSubscriptions()
		l := full.NewLauncher()
		if err := l.Execute(ctx, &launcher.Config{
			AgentLoader:    agent.NewSingleLoader(a),
			SessionService: session.InMemoryService(),
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

func buildAgent(cfg *config.Config, logger *slog.Logger, hugrTransport http.RoundTripper) (agent.Agent, *client.Client, error) {
	hugrClient := client.NewClient(
		cfg.Hugr.URL+"/ipc",
		client.WithTransport(hugrTransport),
	)

	// LLM via Hugr.
	llm := hugrmodel.New(hugrClient, cfg.Agent.Model,
		hugrmodel.WithLogger(logger),
	)

	// Intent-based router. Factory allows config-driven route changes.
	router := intentllm.NewRouter(llm)
	router.WithFactory(func(modelName string) model.LLM {
		return hugrmodel.New(hugrClient, modelName, hugrmodel.WithLogger(logger))
	}).WithLogger(logger)

	// Load YAML config for routes and skill path.
	yamlCfg, err := file.NewConfigProvider("config.yaml")
	if err != nil {
		logger.Debug("config.yaml not loaded", "err", err)
	} else {
		router.LoadRoutesFromConfig(yamlCfg)
		if sp := yamlCfg.GetString("skills.path"); sp != "" {
			cfg.Agent.SkillsPath = sp
		}
		yamlCfg.OnChange(func() {
			logger.Info("config.yaml changed, reloading routes")
			router.LoadRoutesFromConfig(yamlCfg)
		})
	}

	// Constitution (base system prompt).
	constitution, err := os.ReadFile(cfg.Agent.Constitution)
	if err != nil {
		return nil, nil, fmt.Errorf("read constitution %s: %w", cfg.Agent.Constitution, err)
	}
	prompt := hugragent.NewPromptBuilder(string(constitution))

	// Skill catalog for prompt injection.
	skillsPath := cfg.Agent.SkillsPath
	if skillsPath == "" {
		skillsPath = "./skills"
	}
	skillProvider := file.NewSkillProvider(skillsPath)
	skills, err := skillProvider.ListMeta(context.Background())
	if err != nil {
		logger.Warn("failed to list skills", "err", err)
	} else if len(skills) > 0 {
		prompt.SetCatalog(skills)
		logger.Info("skill catalog loaded", "count", len(skills))
	}

	// Dynamic toolset: system tools always available, MCP tools added via skill-load.
	toolset := hugragent.NewDynamicToolset()
	tokens := hugragent.NewTokenEstimator()

	sysDeps := &systemtools.Deps{
		Skills:    skillProvider,
		Prompt:    prompt,
		Toolset:   toolset,
		Tokens:    tokens,
		Transport: hugrTransport,
		Logger:    logger,
	}
	toolset.AddToolset("system", systemtools.NewSystemToolset(sysDeps))

	debug := os.Getenv("LOG_LEVEL") == "debug"

	a, err := hugragent.NewAgent(hugragent.AgentConfig{
		Router:  router,
		Toolset: toolset,
		Prompt:  prompt,
		Tokens:  tokens,
		Logger:  logger,
		Debug:   debug,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("create agent: %w", err)
	}
	return a, hugrClient, nil
}


// buildHugrTransport creates the HTTP transport for all Hugr communication.
//
// Priority:
//  1. HUGR_ACCESS_TOKEN + HUGR_TOKEN_URL → RemoteStore (production)
//  2. HUGR_OIDC_ISSUER + HUGR_OIDC_CLIENT_ID → OIDCStore (dev, browser flow)
//  3. HUGR_SECRET_KEY → static header (dev legacy)
//
// When using OIDC, pass the server mux to register /auth/* callback routes.
func buildHugrTransport(cfg *config.Config, logger *slog.Logger, mux *http.ServeMux) http.RoundTripper {
	// 1. Production: token exchange service.
	if cfg.Hugr.UseTokenAuth() {
		store := auth.NewRemoteStore(cfg.Hugr.AccessToken, cfg.Hugr.TokenURL)
		logger.Info("auth: using remote token exchange", "url", cfg.Hugr.TokenURL)
		return auth.Transport(store, http.DefaultTransport)
	}

	// 2. Dev override: static secret key.
	if cfg.Hugr.SecretKey != "" {
		logger.Info("auth: using secret key (dev)")
		return &headerTransport{
			base:    http.DefaultTransport,
			headers: map[string]string{"x-hugr-secret-key": cfg.Hugr.SecretKey},
		}
	}

	// 3. OIDC browser flow: explicit config or auto-discover from Hugr.
	oidcIssuer := cfg.Hugr.OIDCIssuer
	oidcClientID := cfg.Hugr.OIDCClientID

	if !cfg.Hugr.UseOIDC() && cfg.Hugr.CanDiscoverOIDC() {
		if discovered, err := auth.DiscoverOIDCFromHugr(context.Background(), cfg.Hugr.URL); err != nil {
			logger.Debug("OIDC auto-discovery failed", "err", err)
		} else if discovered != nil {
			oidcIssuer = discovered.Issuer
			oidcClientID = discovered.ClientID
			logger.Info("auth: discovered OIDC from Hugr", "issuer", oidcIssuer)
		}
	}

	if oidcIssuer != "" && oidcClientID != "" {
		store, err := auth.NewOIDCStore(context.Background(), auth.OIDCConfig{
			IssuerURL:   oidcIssuer,
			ClientID:    oidcClientID,
			RedirectURL: cfg.Agent.BaseURL + "/auth/callback",
			Logger:      logger,
		})
		if err != nil {
			logger.Error("OIDC setup failed", "err", err)
		} else {
			if mux != nil {
				store.RegisterCallbackRoute(mux)
			}
			store.PromptLogin()
			logger.Info("auth: using OIDC browser flow", "issuer", oidcIssuer)
			return auth.Transport(store, http.DefaultTransport)
		}
	}

	logger.Warn("auth: no credentials configured — requests to Hugr will be unauthenticated")
	return http.DefaultTransport
}

// a2aHandlers returns the agent card handler and JSON-RPC invoke handler.
func a2aHandlers(a agent.Agent, sessionSvc session.Service, artifactSvc artifact.Service, baseURL string) (cardHandler, invokeHandler http.Handler) {
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

// serveAndShutdown starts the HTTP server and handles graceful shutdown.
func serveAndShutdown(ctx context.Context, srv *http.Server, hugrClient *client.Client, logger *slog.Logger) error {
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		logger.Info("shutting down: draining requests")
		srv.Shutdown(shutdownCtx)
		logger.Info("shutting down: closing subscriptions")
		hugrClient.CloseSubscriptions()
	}()
	return srv.ListenAndServe()
}

// runA2A starts the A2A server (default mode).
func runA2A(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	sessionSvc := session.InMemoryService()
	artifactSvc := artifact.InMemoryService()

	smux := http.NewServeMux()
	hugrTransport := buildHugrTransport(cfg, logger, smux)

	a, hugrClient, err := buildAgent(cfg, logger, hugrTransport)
	if err != nil {
		return fmt.Errorf("build agent: %w", err)
	}

	cardH, invokeH := a2aHandlers(a, sessionSvc, artifactSvc, cfg.Agent.BaseURL)
	smux.Handle(a2asrv.WellKnownAgentCardPath, cardH)
	smux.Handle("/invoke", invokeH)

	srv := &http.Server{Addr: fmt.Sprintf(":%d", cfg.Agent.Port), Handler: smux}
	logger.Info("A2A server listening",
		"addr", srv.Addr,
		"invoke", cfg.Agent.BaseURL+"/invoke",
	)
	return serveAndShutdown(ctx, srv, hugrClient, logger)
}

// runWithDevUI starts A2A + ADK REST API + dev UI.
func runWithDevUI(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	sessionSvc := session.InMemoryService()
	artifactSvc := artifact.InMemoryService()

	router := mux.NewRouter()

	// OIDC callback routes on a separate ServeMux (gorilla/mux can't use http.ServeMux patterns).
	authMux := http.NewServeMux()
	hugrTransport := buildHugrTransport(cfg, logger, authMux)
	router.PathPrefix("/auth/").Handler(authMux)

	a, hugrClient, err := buildAgent(cfg, logger, hugrTransport)
	if err != nil {
		return fmt.Errorf("build agent: %w", err)
	}

	// A2A.
	cardH, invokeH := a2aHandlers(a, sessionSvc, artifactSvc, cfg.Agent.BaseURL)
	router.Handle(a2asrv.WellKnownAgentCardPath, cardH)
	router.Handle("/invoke", invokeH)

	// ADK REST API + dev UI.
	agentLoader := agent.NewSingleLoader(a)
	memorySvc := memory.InMemoryService()
	apiServer, err := adkrest.NewServer(adkrest.ServerConfig{
		SessionService:  sessionSvc,
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
		corsMiddleware(http.StripPrefix("/api", apiServer)),
	)

	ui := webui.NewLauncher()
	apiAddr := cfg.Agent.BaseURL + "/api"
	if _, err := ui.Parse([]string{"-api_server_address", apiAddr}); err != nil {
		return fmt.Errorf("parse webui flags: %w", err)
	}
	if err := ui.SetupSubrouters(router, &launcher.Config{
		SessionService:  sessionSvc,
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
	return serveAndShutdown(ctx, srv, hugrClient, logger)
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	return t.base.RoundTrip(req)
}
