package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/artifact"
	"google.golang.org/adk/cmd/launcher"
	"google.golang.org/adk/cmd/launcher/web/webui"
	"google.golang.org/adk/memory"
	"google.golang.org/adk/server/adkrest"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/devui"
)

// serveDevUI runs A2A endpoints on cfg.A2A.Port (same as prod) and
// the ADK webui + REST + /dev helpers on cfg.DevUI.Port, loopback-
// only. Two listeners, one runtime.
func serveDevUI(ctx context.Context, a *app) error {
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
		DebugConfig:     adkrest.DebugTelemetryConfig{},
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
