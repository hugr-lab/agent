// Package main is the entry point for the hugr-agent runtime.
//
// Startup flow:
//
//  1. Load bootstrap config (.env → HUGR_URL, HUGR_ACCESS_TOKEN, …).
//  2. Bring up the full runtime once, independent of mode:
//     auth SourceRegistry on a shared mux, hugr client, full
//     config (local YAML or remote hub pull), ADK agent,
//     scheduler, classifier, memory services.
//  3. Dispatch to one of three mode handlers based on os.Args[1]:
//     a2a (default), devui, console. Each handler only wires its
//     mode-specific HTTP layout on top of the prepared runtime.
//  4. Block on ctx until SIGINT/SIGTERM, then shut down cleanly.
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
	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/devui"
	"github.com/hugr-lab/query-engine/client"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/artifact"
	"google.golang.org/adk/cmd/launcher"
	"google.golang.org/adk/cmd/launcher/full"
	"google.golang.org/adk/cmd/launcher/web/webui"
	"google.golang.org/adk/memory"
	"google.golang.org/adk/server/adkrest"
)

// app bundles everything bootstrap produces. Mode-specific serve*
// helpers read from it; they never re-touch the startup phases.
type app struct {
	boot    *config.BootstrapConfig
	cfg     *config.Config
	logger  *slog.Logger
	runtime *agentRuntime
	authReg *auth.SourceRegistry

	// authMux has /auth/callback + per-Source /auth/login/<name>
	// mounted by auth.SourceRegistry.Mount. Mode handlers add their
	// own routes (agent card, invoke, admin) on the same mux.
	authMux *http.ServeMux

	// prompts are OIDC prompt-login hooks to fire once the HTTP
	// listener for authMux is bound.
	prompts []func()
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	boot, err := config.LoadBootstrap(".env")
	if err != nil {
		log.Fatalf("bootstrap: %v", err)
	}

	logger := newLogger()
	logger.Info("hugr-agent starting",
		"hugr_url", boot.Hugr.URL,
		"mode", modeLabel(boot),
	)

	a, err := bootstrap(ctx, boot, logger)
	if err != nil {
		log.Fatalf("bootstrap: %v", err)
	}
	defer a.runtime.close(logger)

	sub := ""
	if len(os.Args) > 1 {
		sub = os.Args[1]
	}

	switch sub {
	case "console":
		err = serveConsole(ctx, a)
	case "devui":
		err = serveDevUI(ctx, a)
	default:
		err = serveA2A(ctx, a)
	}
	if err != nil && ctx.Err() == nil {
		log.Fatalf("%s: %v", sub, err)
	}

	logger.Info("shutdown complete")
}

// bootstrap brings up every non-HTTP long-lived component:
// SourceRegistry, hugr client, full config, ADK agent + runtime.
// The returned *app owns all of it; caller closes via
// app.runtime.close(logger).
func bootstrap(ctx context.Context, boot *config.BootstrapConfig, logger *slog.Logger) (*app, error) {
	authMux := http.NewServeMux()

	authReg, hugrTransport, err := buildAuthForBootstrap(ctx, boot, authMux, logger)
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}

	hugrClient := client.NewClient(
		boot.Hugr.URL+"/ipc",
		client.WithTransport(hugrTransport),
	)

	cfg, err := loadFullConfig(ctx, boot, hugrClient, logger)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	if err := registerProviderAuth(ctx, cfg, authReg, logger); err != nil {
		return nil, fmt.Errorf("provider auth: %w", err)
	}

	logger.Info("agent configured",
		"model", cfg.LLM.Model,
		"local_db", cfg.LocalDBEnabled,
	)

	rt, err := buildRuntime(ctx, boot, cfg, logger, authReg, hugrClient)
	if err != nil {
		return nil, err
	}

	return &app{
		boot:    boot,
		cfg:     cfg,
		logger:  logger,
		runtime: rt,
		authReg: authReg,
		authMux: authMux,
		prompts: authReg.PromptLogins(),
	}, nil
}

// registerProviderAuth is Phase B.5: adds cfg.Auth provider entries
// (excluding the already-registered primary Source) to the existing
// registry. Entries with type=hugr become aliases onto the primary.
func registerProviderAuth(ctx context.Context, cfg *config.Config, reg *auth.SourceRegistry, logger *slog.Logger) error {
	primary := reg.Primary()
	specs := make([]auth.AuthSpec, 0, len(cfg.Auth))
	for _, a := range cfg.Auth {
		if a.Name == primary {
			// Primary Source is already registered. Provider entries
			// that want to reuse it should declare a distinct name
			// with type=hugr (alias resolution in BuildSources).
			continue
		}
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
	return auth.BuildSources(ctx, specs, reg, logger)
}

// ---------------------------------------------------------------------
// Mode dispatch
// ---------------------------------------------------------------------

// serveA2A is the default mode: agent card + invoke + admin routes on
// a single A2A listener. OIDC callbacks live on the same mux, so the
// redirect_uri matches prod.
func serveA2A(ctx context.Context, a *app) error {
	// Spec 008: a.runtime.Artifacts is the persistent registry that
	// implements adk artifact.Service directly. The previous
	// artifact.InMemoryService() stub is gone.
	artifactSvc := a.runtime.Artifacts
	attachA2A(a, a.authMux, a.runtime, artifactSvc, a.cfg.A2A.BaseURL)

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", a.cfg.A2A.Port),
		Handler: a.authMux,
	}
	a.logger.Info("A2A server listening",
		"addr", srv.Addr,
		"invoke", a.cfg.A2A.BaseURL+"/invoke",
	)
	return serve(ctx, []*http.Server{srv}, a.prompts, a.logger)
}

// serveDevUI runs A2A endpoints on cfg.A2A.Port (same as prod) and
// the ADK webui + REST + /dev helpers on cfg.DevUI.Port, loopback-
// only. Two listeners, one runtime.
func serveDevUI(ctx context.Context, a *app) error {
	// Spec 008: a.runtime.Artifacts is the persistent registry that
	// implements adk artifact.Service directly. The previous
	// artifact.InMemoryService() stub is gone.
	artifactSvc := a.runtime.Artifacts
	attachA2A(a, a.authMux, a.runtime, artifactSvc, a.cfg.A2A.BaseURL)

	devHandler, err := buildDevRouter(a.runtime, artifactSvc,
		a.authReg.TokenStores(), a.cfg.DevUI.BaseURL, a.cfg.A2A.BaseURL)
	if err != nil {
		return fmt.Errorf("devui: %w", err)
	}

	a2aSrv := &http.Server{
		Addr:    fmt.Sprintf(":%d", a.cfg.A2A.Port),
		Handler: a.authMux,
	}
	devSrv := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", a.cfg.DevUI.Port),
		Handler: devHandler,
	}
	a.logger.Info("devui: A2A listener",
		"addr", a2aSrv.Addr, "invoke", a.cfg.A2A.BaseURL+"/invoke")
	a.logger.Info("devui: UI + REST listener (loopback only)",
		"addr", devSrv.Addr,
		"ui", a.cfg.DevUI.BaseURL+"/",
		"token", a.cfg.DevUI.BaseURL+"/dev/token")

	return serve(ctx, []*http.Server{a2aSrv, devSrv}, a.prompts, a.logger)
}

// serveConsole launches the ADK full-launcher REPL on stdin/stdout.
// The auth mux is served on cfg.A2A.Port so OIDC callbacks reach us
// even from a browser in another session.
func serveConsole(ctx context.Context, a *app) error {
	addr := fmt.Sprintf(":%d", a.cfg.A2A.Port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	go func() {
		if err := http.Serve(listener, a.authMux); err != nil &&
			err != http.ErrServerClosed {
			a.logger.Error("auth callback server", "err", err)
		}
	}()
	for _, p := range a.prompts {
		go p()
	}

	l := full.NewLauncher()
	if err := l.Execute(ctx, &launcher.Config{
		AgentLoader:    agent.NewSingleLoader(a.runtime.Agent),
		SessionService: a.runtime.Sessions,
	}, os.Args[2:]); err != nil {
		return fmt.Errorf("launcher: %w\n%s", err, l.CommandLineSyntax())
	}
	return nil
}

// ---------------------------------------------------------------------
// HTTP mount helpers
// ---------------------------------------------------------------------

// attachA2A wires the agent card, /invoke, and /admin/* routes onto
// the given mux. Idempotent only if called at most once per mux —
// http.ServeMux rejects duplicate registrations.
//
// The runtime's artifact manager doubles as the user-upload ADK
// plugin source: BuildHandlers wires the plugin into runner.Config so
// incoming A2A FilePart{FileBytes} land in the registry with a rich
// text placeholder before the LLM ever sees the bytes (US10).
func attachA2A(a *app, mux *http.ServeMux, rt *agentRuntime, artifacts artifact.Service, baseURL string) {
	uploadPlugin, err := rt.Artifacts.UserUploadPlugin()
	if err != nil {
		a.logger.Warn("artifacts: user-upload plugin disabled", "err", err)
	}
	cardH, invokeH := a2a.BuildHandlers(rt.Agent, rt.Sessions, artifacts, uploadPlugin, baseURL)
	mux.Handle(a2asrv.WellKnownAgentCardPath, cardH)
	mux.Handle("/invoke", invokeH)
	registerAdminRoutes(mux, rt.Tools, rt.Skills)
	registerArtifactDownload(mux, rt.Artifacts, artifactDownloadConfig{
		MaxBytes:     a.cfg.Artifacts.DownloadMaxBytes,
		WriteChunk:   a.cfg.Artifacts.DownloadWriteChunk,
		WriteTimeout: a.cfg.Artifacts.DownloadWriteTimeout,
	}, a.logger)
}

// buildDevRouter assembles the ADK webui + REST API + dev helpers
// onto a gorilla/mux router. Returned as an http.Handler so the
// caller just hands it to an http.Server.
func buildDevRouter(rt *agentRuntime, artifacts artifact.Service,
	tokens map[string]auth.TokenStore, devBaseURL, a2aBaseURL string) (http.Handler, error) {

	router := mux.NewRouter()
	agentLoader := agent.NewSingleLoader(rt.Agent)

	apiServer, err := adkrest.NewServer(adkrest.ServerConfig{
		SessionService:  rt.Sessions,
		ArtifactService: artifacts,
		MemoryService:   memory.InMemoryService(),
		AgentLoader:     agentLoader,
		SSEWriteTimeout: 120 * time.Second,
		DebugConfig:     &adkrest.DebugTelemetryConfig{},
	})
	if err != nil {
		return nil, fmt.Errorf("create REST server: %w", err)
	}
	router.PathPrefix("/api").Handler(
		corsMiddleware(devBaseURL, http.StripPrefix("/api", apiServer)),
	)

	router.Handle("/dev/token", devui.TokenHandler(tokens))
	router.Handle("/dev/auth/trigger", devui.TriggerAuthHandler(a2aBaseURL))

	ui := webui.NewLauncher()
	if _, err := ui.Parse([]string{"-api_server_address", devBaseURL + "/api"}); err != nil {
		return nil, fmt.Errorf("parse webui flags: %w", err)
	}
	if err := ui.SetupSubrouters(router, &launcher.Config{
		SessionService:  rt.Sessions,
		ArtifactService: artifacts,
		AgentLoader:     agentLoader,
	}); err != nil {
		return nil, fmt.Errorf("setup webui: %w", err)
	}
	return router, nil
}

// ---------------------------------------------------------------------
// HTTP lifecycle
// ---------------------------------------------------------------------

// serve binds a listener for each server, fires prompt-login hooks
// once every listener is live, and blocks until ctx is cancelled or
// one of the servers errors. On ctx cancel it triggers graceful
// shutdown on every server concurrently.
//
// Returns the first non-trivial serve error; http.ErrServerClosed is
// treated as a clean exit.
func serve(ctx context.Context, servers []*http.Server, prompts []func(), logger *slog.Logger) error {
	if len(servers) == 0 {
		return fmt.Errorf("serve: no servers")
	}

	type result struct {
		err  error
		addr string
	}
	results := make(chan result, len(servers))
	listeners := make([]net.Listener, 0, len(servers))

	for _, srv := range servers {
		ln, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			for _, l := range listeners {
				_ = l.Close()
			}
			return fmt.Errorf("listen %s: %w", srv.Addr, err)
		}
		listeners = append(listeners, ln)
		s := srv
		go func() {
			err := s.Serve(ln)
			results <- result{err: err, addr: s.Addr}
		}()
	}
	for _, p := range prompts {
		go p()
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		logger.Info("shutting down: draining requests")
		for _, s := range servers {
			_ = s.Shutdown(shutdownCtx)
		}
	}()

	var firstErr error
	for range servers {
		r := <-results
		if r.err != nil && r.err != http.ErrServerClosed && firstErr == nil {
			firstErr = fmt.Errorf("serve %s: %w", r.addr, r.err)
		}
	}
	return firstErr
}

// ---------------------------------------------------------------------
// Misc helpers
// ---------------------------------------------------------------------

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

func modeLabel(boot *config.BootstrapConfig) string {
	if boot.Remote() {
		return "remote"
	}
	return "local"
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
