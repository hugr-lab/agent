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
	"github.com/hugr-lab/hugen/pkg/config"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/artifact"
	"google.golang.org/adk/cmd/launcher"
	"google.golang.org/adk/cmd/launcher/full"
	"google.golang.org/adk/cmd/launcher/web/webui"
	"google.golang.org/adk/memory"
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
