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
	"github.com/hugr-lab/agent/internal/config"
	"github.com/hugr-lab/agent/pkg/auth"
	"github.com/hugr-lab/agent/pkg/hugrmodel"
	"github.com/hugr-lab/query-engine/client"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/artifact"
	"google.golang.org/adk/cmd/launcher"
	"google.golang.org/adk/cmd/launcher/full"
	"google.golang.org/adk/cmd/launcher/web/webui"
	"google.golang.org/adk/memory"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/server/adka2a"
	"google.golang.org/adk/server/adkrest"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/mcptoolset"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
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

	llm := hugrmodel.New(hugrClient, cfg.Agent.Model,
		hugrmodel.WithLogger(logger),
	)

	constitution, err := os.ReadFile(cfg.Agent.Constitution)
	if err != nil {
		return nil, nil, fmt.Errorf("read constitution %s: %w", cfg.Agent.Constitution, err)
	}

	mcpTransport := &sdkmcp.StreamableClientTransport{
		Endpoint:             cfg.Hugr.MCPUrl,
		DisableStandaloneSSE: true,
		HTTPClient:           &http.Client{Transport: hugrTransport},
	}

	mcpTools, err := mcptoolset.New(mcptoolset.Config{
		Transport: mcpTransport,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("create MCP toolset: %w", err)
	}

	a, err := llmagent.New(llmagent.Config{
		Name:        "hugr_agent",
		Description: "Hugr Data Mesh Agent — explores data sources, builds queries, presents results",
		Model:       llm,
		Instruction: string(constitution),
		Toolsets:    []tool.Toolset{mcpTools},
	})
	if err != nil {
		return nil, nil, err
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
			RedirectURL: fmt.Sprintf("http://localhost:%d/auth/callback", cfg.Agent.Port),
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

// setupA2A registers A2A endpoints on the mux.
func setupA2A(mux *http.ServeMux, a agent.Agent, sessionSvc session.Service, artifactSvc artifact.Service, port int) {
	agentCard := &a2a.AgentCard{
		Name:               a.Name(),
		Description:        a.Description(),
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		URL:                fmt.Sprintf("http://localhost:%d/invoke", port),
		PreferredTransport: a2a.TransportProtocolJSONRPC,
		Skills:             adka2a.BuildAgentSkills(a),
		Capabilities:       a2a.AgentCapabilities{Streaming: true},
	}
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(agentCard))

	executor := adka2a.NewExecutor(adka2a.ExecutorConfig{
		RunnerConfig: runner.Config{
			AppName:         a.Name(),
			Agent:           a,
			SessionService:  sessionSvc,
			ArtifactService: artifactSvc,
		},
	})
	reqHandler := a2asrv.NewHandler(executor)
	mux.Handle("/invoke", a2asrv.NewJSONRPCHandler(reqHandler))
}

// runA2A starts the A2A server (default mode).
func runA2A(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	sessionSvc := session.InMemoryService()
	artifactSvc := artifact.InMemoryService()

	mux := http.NewServeMux()
	hugrTransport := buildHugrTransport(cfg, logger, mux)

	a, hugrClient, err := buildAgent(cfg, logger, hugrTransport)
	if err != nil {
		return fmt.Errorf("build agent: %w", err)
	}
	setupA2A(mux, a, sessionSvc, artifactSvc, cfg.Agent.Port)

	srv := &http.Server{Addr: fmt.Sprintf(":%d", cfg.Agent.Port), Handler: mux}
	logger.Info("A2A server listening",
		"addr", srv.Addr,
		"invoke", fmt.Sprintf("http://localhost:%d/invoke", cfg.Agent.Port),
		"card", fmt.Sprintf("http://localhost:%d%s", cfg.Agent.Port, a2asrv.WellKnownAgentCardPath),
	)

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

// runWithDevUI starts A2A + ADK REST API + dev UI.
func runWithDevUI(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	sessionSvc := session.InMemoryService()
	artifactSvc := artifact.InMemoryService()

	router := mux.NewRouter()

	authMux := http.NewServeMux()
	hugrTransport := buildHugrTransport(cfg, logger, authMux)
	router.PathPrefix("/auth/").Handler(authMux)

	a, hugrClient, err := buildAgent(cfg, logger, hugrTransport)
	if err != nil {
		return fmt.Errorf("build agent: %w", err)
	}
	agentLoader := agent.NewSingleLoader(a)

	// A2A.
	agentCard := &a2a.AgentCard{
		Name:               a.Name(),
		Description:        a.Description(),
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		URL:                fmt.Sprintf("http://localhost:%d/invoke", cfg.Agent.Port),
		PreferredTransport: a2a.TransportProtocolJSONRPC,
		Skills:             adka2a.BuildAgentSkills(a),
		Capabilities:       a2a.AgentCapabilities{Streaming: true},
	}
	router.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(agentCard))

	executor := adka2a.NewExecutor(adka2a.ExecutorConfig{
		RunnerConfig: runner.Config{
			AppName:         a.Name(),
			Agent:           a,
			SessionService:  sessionSvc,
			ArtifactService: artifactSvc,
		},
	})
	reqHandler := a2asrv.NewHandler(executor)
	router.Handle("/invoke", a2asrv.NewJSONRPCHandler(reqHandler))

	// ADK REST API at /api.
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

	// Dev UI.
	ui := webui.NewLauncher()
	apiAddr := fmt.Sprintf("http://localhost:%d/api", cfg.Agent.Port)
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
		"invoke", fmt.Sprintf("http://localhost:%d/invoke", cfg.Agent.Port),
		"ui", fmt.Sprintf("http://localhost:%d/", cfg.Agent.Port),
	)

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
