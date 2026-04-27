package main

import (
	"context"
	"fmt"
	"net/http"

	"github.com/a2aproject/a2a-go/a2asrv"
	"google.golang.org/adk/artifact"

	"github.com/hugr-lab/hugen/pkg/a2a"
)

// serveA2A is the default mode: agent card + invoke + admin routes on
// a single A2A listener. OIDC callbacks live on the same mux, so the
// redirect_uri matches prod.
func serveA2A(ctx context.Context, a *app) error {
	// Spec 008: a.runtime.Artifacts is the persistent registry that
	// implements adk artifact.Service directly.
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
