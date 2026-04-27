// Package main is the entry point for the hugr-agent runtime.
//
// Bootstrap is a strictly-ordered sequence of nine phases (see
// numbered comments below). Each phase has its own helper file
// (auth.go, hugr.go, identity.go, config.go, models.go, runtime.go)
// so the order is visible at a glance from main() and side effects
// are obvious.
//
//  1. Load BootstrapConfig from .env.
//  2. Build the logger.
//  3. Build the auth Service (mounts /auth/callback + /auth/login on
//     a fresh authMux that the mode handlers will share).
//  4. Connect the remote hugr client through the auth-bearing transport
//     (nil when no HUGR_URL is set).
//  5. Build identity.Source — hub | local | local+hub depending on
//     mode. WhoAmI / Permission resolve through this.
//  6. Build RuntimeConfig — yaml or hub-pulled, identity-driven.
//     registerProviderAuth fires once cfg.Auth is known.
//  7. Build the local engine (when cfg.LocalDBEnabled) → localQuerier.
//  8. Build the model router from local + remote queriers.
//  9. Build the agent runtime from all the assembled parts.
//
// Mode dispatch (a2a, devui) then runs the chosen serve* helper on
// top of the prepared runtime; no phase is re-entered after dispatch.
package main

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/hugr-lab/hugen/pkg/auth"
	hugenruntime "github.com/hugr-lab/hugen/pkg/runtime"
	qetypes "github.com/hugr-lab/query-engine/types"
)

// app bundles everything bootstrap produces. Mode-specific serve*
// helpers read from it; they never re-touch the startup phases.
type app struct {
	boot    *hugenruntime.BootstrapConfig
	cfg     *hugenruntime.RuntimeConfig
	logger  *slog.Logger
	runtime *agentRuntime
	authReg *auth.Service

	// authMux has /auth/callback + per-Source /auth/login/<name>
	// mounted by auth.Service. Mode handlers add their own routes
	// (agent card, invoke, admin) on the same mux.
	authMux *http.ServeMux

	// prompts are OIDC prompt-login hooks to fire once the HTTP
	// listener for authMux is bound.
	prompts []func()
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Phase 1: bootstrap config from .env.
	boot, err := hugenruntime.LoadBootstrap(".env")
	if err != nil {
		log.Fatalf("bootstrap: %v", err)
	}

	// Phase 2: logger.
	logger := newLogger()
	logger.Info("hugr-agent starting", "hugr_url", boot.Hugr.URL, "mode", modeLabel(boot))

	// Phase 3: auth service on a fresh shared mux.
	authMux := http.NewServeMux()
	authReg, err := buildAuthService(ctx, boot, authMux, logger)
	if err != nil {
		log.Fatalf("auth: %v", err)
	}

	// Phase 4: remote hugr client (nil when no HUGR_URL configured).
	hugrClient := connectRemote(boot, authReg, logger)
	var remoteQuerier qetypes.Querier
	if hugrClient != nil {
		remoteQuerier = hugrClient
	}

	// Phase 5: identity.Source — selected by mode.
	identitySrc := buildIdentity(boot, remoteQuerier, "config.yaml")

	// Phase 6: full RuntimeConfig + register provider auth from
	// cfg.Auth (now that it's known).
	cfg, err := buildRuntimeConfig(ctx, boot, identitySrc, logger)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if err := registerProviderAuth(ctx, cfg, authReg, logger); err != nil {
		log.Fatalf("provider auth: %v", err)
	}
	logger.Info("agent configured", "model", cfg.LLM.Model, "local_db", cfg.LocalDBEnabled)

	// Phase 7: local engine (only when cfg.LocalDBEnabled).
	localEngine, localModels, err := hugenruntime.BuildLocalEngine(ctx, cfg, logger)
	if err != nil {
		log.Fatalf("local engine: %v", err)
	}
	var localQuerier qetypes.Querier
	if localEngine != nil {
		localQuerier = localEngine
	}

	// Phase 8: model router across local + remote queriers.
	router := buildModelRouter(localQuerier, remoteQuerier, localModels, cfg, logger)

	// Phase 9: agent runtime.
	rt, err := buildRuntime(ctx, cfg, runtimeInputs{
		LocalQuerier:  localQuerier,
		RemoteQuerier: remoteQuerier,
		LocalEngine:   localEngine,
		Router:        router,
		AuthService:   authReg,
		HugrClient:    hugrClient,
	}, logger)
	if err != nil {
		log.Fatalf("runtime: %v", err)
	}
	defer rt.close(logger)

	a := &app{
		boot:    boot,
		cfg:     cfg,
		logger:  logger,
		runtime: rt,
		authReg: authReg,
		authMux: authMux,
		prompts: authReg.PromptLogins(),
	}

	sub := ""
	if len(os.Args) > 1 {
		sub = os.Args[1]
	}
	switch sub {
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

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

func modeLabel(boot *hugenruntime.BootstrapConfig) string {
	if boot.Remote() {
		return "remote"
	}
	return "local"
}
