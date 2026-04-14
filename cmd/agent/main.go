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
	"github.com/hugr-lab/hugen/adapters/file"
	"github.com/hugr-lab/hugen/internal/config"
	hugen "github.com/hugr-lab/hugen/pkg/agent"
	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/llms/intent"
	"github.com/hugr-lab/hugen/pkg/models/hugr"
	"github.com/hugr-lab/hugen/pkg/tools/system"
	"github.com/hugr-lab/query-engine/client"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
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
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool/mcptoolset"
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
		if sp := yamlCfg.GetString("skills.path"); sp != "" {
			cfg.Agent.SkillsPath = sp
		}
	}

	// Dynamic toolset: system tools always available, MCP tools added at startup.
	toolset := hugen.NewDynamicToolset()

	// LLM via Hugr.
	llm := hugr.New(hugrClient, cfg.Agent.Model,
		hugr.WithLogger(logger),
		hugr.WithMaxTokens(cfg.Agent.MaxTokens),
		hugr.WithToolChoiceFunc(func() string {
			return "auto"
		}),
	)

	// Intent-based router. Factory allows config-driven route changes.
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

	// Constitution (base system prompt).
	constitution, err := os.ReadFile(cfg.Agent.Constitution)
	if err != nil {
		return nil, nil, fmt.Errorf("read constitution %s: %w", cfg.Agent.Constitution, err)
	}
	prompt := hugen.NewPromptBuilder(string(constitution))

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

	// Pre-load Hugr MCP tools at startup (ADK caches tools per invocation,
	// so DynamicToolset changes during skill_load don't take effect).
	mcpEndpoint := os.Getenv("HUGR_MCP_URL")
	if mcpEndpoint != "" {
		mcpTransport := &sdkmcp.StreamableClientTransport{
			Endpoint:             mcpEndpoint,
			DisableStandaloneSSE: true,
			HTTPClient:           &http.Client{Transport: hugrTransport},
		}
		mcpTools, err := mcptoolset.New(mcptoolset.Config{
			Transport: mcpTransport,
		})
		if err != nil {
			logger.Error("MCP tools connection failed", "endpoint", mcpEndpoint, "err", err)
		} else {
			toolset.AddToolset("mcp:hugr", mcpTools)
			logger.Info("MCP tools connected", "endpoint", mcpEndpoint)
		}
	} else {
		logger.Warn("HUGR_MCP_URL not set — MCP tools unavailable")
	}

	tokens := hugen.NewTokenEstimator()

	sysDeps := &system.Deps{
		Skills:    skillProvider,
		Prompt:    prompt,
		Toolset:   toolset,
		Tokens:    tokens,
		Transport: hugrTransport,
		Logger:    logger,
	}
	toolset.AddToolset("system", system.NewSystemToolset(sysDeps))

	debug := os.Getenv("LOG_LEVEL") == "debug"

	a, err := hugen.NewAgent(hugen.AgentConfig{
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
