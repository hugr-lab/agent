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

	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/gorilla/mux"
	"github.com/hugr-lab/hugen/pkg/a2a"
	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/devui"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/artifact"
	"google.golang.org/adk/cmd/launcher"
	"google.golang.org/adk/cmd/launcher/full"
	"google.golang.org/adk/cmd/launcher/web/webui"
	"google.golang.org/adk/memory"
	"google.golang.org/adk/server/adkrest"
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
		if err := runConsole(ctx, cfg, logger); err != nil && ctx.Err() == nil {
			log.Fatalf("Console error: %v", err)
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

// runConsole launches the ADK full-launcher REPL. Auth callbacks live on
// the regular A2A port (10000) so OIDC's redirect_uri is configured
// once regardless of mode.
func runConsole(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	consoleMux := http.NewServeMux()
	authStores, err := buildAuthStores(ctx, cfg, logger, consoleMux)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	hugrTransport := resolveHugrTransport(cfg, authStores, logger)

	addr := fmt.Sprintf(":%d", cfg.Agent.Port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
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
		return err
	}
	defer rt.close(logger)
	l := full.NewLauncher()
	if err := l.Execute(ctx, &launcher.Config{
		AgentLoader:    agent.NewSingleLoader(rt.agent),
		SessionService: rt.sessions,
	}, os.Args[2:]); err != nil {
		return fmt.Errorf("launcher: %w\n%s", err, l.CommandLineSyntax())
	}
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

// serveMany runs every *http.Server concurrently on its own listener
// and waits until every one exits. ctx.Done triggers graceful shutdown
// on all servers; rt.close runs once every listener has drained.
func serveMany(ctx context.Context, servers []*http.Server, rt *agentRuntime, postListen []func(), logger *slog.Logger) error {
	type result struct {
		err  error
		addr string
	}
	results := make(chan result, len(servers))
	listeners := make([]net.Listener, 0, len(servers))

	for _, srv := range servers {
		listener, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			for _, l := range listeners {
				l.Close()
			}
			return fmt.Errorf("listen %s: %w", srv.Addr, err)
		}
		listeners = append(listeners, listener)
		s := srv
		go func() {
			err := s.Serve(listener)
			results <- result{err: err, addr: s.Addr}
		}()
	}
	for _, p := range postListen {
		go p()
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		logger.Info("shutting down: draining requests")
		for _, s := range servers {
			s.Shutdown(shutdownCtx)
		}
		rt.close(logger)
	}()

	// Wait for every server to return; surface the first non-nil,
	// non-ErrServerClosed error.
	var firstErr error
	for i := 0; i < len(servers); i++ {
		r := <-results
		if r.err != nil && r.err != http.ErrServerClosed && firstErr == nil {
			firstErr = fmt.Errorf("serve %s: %w", r.addr, r.err)
		}
	}
	return firstErr
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

	cardH, invokeH := a2a.BuildHandlers(rt.agent, rt.sessions, artifactSvc, cfg.Agent.BaseURL)
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

// runWithDevUI runs the A2A endpoints on cfg.Agent.Port (same as prod)
// and the ADK webui + REST + /dev helpers on cfg.Agent.DevUIPort,
// loopback-only. Two listeners, one runtime.
func runWithDevUI(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	artifactSvc := artifact.InMemoryService()

	// A2A listener mux — OIDC callbacks are registered here so the
	// redirect_uri is the same as in prod (independent of mode).
	a2aMux := http.NewServeMux()
	authStores, err := buildAuthStores(ctx, cfg, logger, a2aMux)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	hugrTransport := resolveHugrTransport(cfg, authStores, logger)

	rt, err := buildRuntime(ctx, cfg, logger, authStores, hugrTransport)
	if err != nil {
		return err
	}

	cardH, invokeH := a2a.BuildHandlers(rt.agent, rt.sessions, artifactSvc, cfg.Agent.BaseURL)
	a2aMux.Handle(a2asrv.WellKnownAgentCardPath, cardH)
	a2aMux.Handle("/invoke", invokeH)
	registerAdminRoutes(a2aMux, rt.tools, rt.skills)

	// DevUI listener — separate gorilla/mux router for ADK webui's
	// sub-router plumbing.
	devRouter := mux.NewRouter()

	agentLoader := agent.NewSingleLoader(rt.agent)
	apiServer, err := adkrest.NewServer(adkrest.ServerConfig{
		SessionService:  rt.sessions,
		ArtifactService: artifactSvc,
		MemoryService:   memory.InMemoryService(),
		AgentLoader:     agentLoader,
		SSEWriteTimeout: 120 * time.Second,
		DebugConfig:     &adkrest.DebugTelemetryConfig{},
	})
	if err != nil {
		return fmt.Errorf("create REST server: %w", err)
	}
	devRouter.PathPrefix("/api").Handler(
		corsMiddleware(cfg.Agent.DevUIBaseURL, http.StripPrefix("/api", apiServer)),
	)

	// /dev/token → JSON {access_token, name, type}; /dev/auth/trigger
	// → 302 to A2A listener's /auth/<name>/login for re-login.
	devRouter.Handle("/dev/token", devui.TokenHandler(authStores.Tokens))
	devRouter.Handle("/dev/auth/trigger", devui.TriggerAuthHandler(cfg.Agent.BaseURL))

	ui := webui.NewLauncher()
	apiAddr := cfg.Agent.DevUIBaseURL + "/api"
	if _, err := ui.Parse([]string{"-api_server_address", apiAddr}); err != nil {
		return fmt.Errorf("parse webui flags: %w", err)
	}
	if err := ui.SetupSubrouters(devRouter, &launcher.Config{
		SessionService:  rt.sessions,
		ArtifactService: artifactSvc,
		AgentLoader:     agentLoader,
	}); err != nil {
		return fmt.Errorf("setup webui: %w", err)
	}

	a2aSrv := &http.Server{Addr: fmt.Sprintf(":%d", cfg.Agent.Port), Handler: a2aMux}
	devSrv := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", cfg.Agent.DevUIPort),
		Handler: devRouter,
	}
	logger.Info("devui: A2A listener",
		"addr", a2aSrv.Addr, "invoke", cfg.Agent.BaseURL+"/invoke")
	logger.Info("devui: UI + REST listener (loopback only)",
		"addr", devSrv.Addr,
		"ui", cfg.Agent.DevUIBaseURL+"/",
		"token", cfg.Agent.DevUIBaseURL+"/dev/token")

	return serveMany(ctx, []*http.Server{a2aSrv, devSrv}, rt, authStores.PromptLogin, logger)
}

// corsMiddleware allows the DevUI listener's SPA (served from the same
// origin) and loopback clients to call the /api endpoints without a
// preflight rejection. For non-localhost origins it restricts to the
// exact DevUI base URL.
func corsMiddleware(baseURL string, next http.Handler) http.Handler {
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
