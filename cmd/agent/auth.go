package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/hugr-lab/hugen/pkg/auth"
	hugrauth "github.com/hugr-lab/hugen/pkg/auth/sources/hugr"
	oidcauth "github.com/hugr-lab/hugen/pkg/auth/sources/oidc"
	hugenruntime "github.com/hugr-lab/hugen/pkg/runtime"
)

// buildAuthService is Phase 3: builds the single hugr Source declared
// by .env, wires it into a fresh auth.Service (which mounts the shared
// /auth/callback + /auth/login/{name} routes on mux at construction
// time), and returns the Service. Callers retrieve a token-bearing
// http transport via auth.Transport(svc.TokenStore("hugr")…) once the
// Service is ready.
func buildAuthService(ctx context.Context, boot *hugenruntime.BootstrapConfig, mux *http.ServeMux, logger *slog.Logger) (*auth.Service, error) {
	svc := auth.NewService(logger, mux)
	if boot.Hugr.URL == "" {
		logger.Info("auth: no HUGR_URL configured; skipping hugr source")
		return svc, nil
	}

	hugrSrc, err := hugrauth.BuildHugrSource(ctx, hugrauth.Config{
		AccessToken: boot.HugrAuth.AccessToken,
		TokenURL:    boot.HugrAuth.TokenURL,
		Issuer:      boot.HugrAuth.Issuer,
		ClientID:    boot.HugrAuth.ClientID,
		BaseURI:     boot.A2A.BaseURL,
		DiscoverURL: boot.Hugr.URL,
	}, logger)
	if err != nil {
		return nil, fmt.Errorf("hugr source: %w", err)
	}
	if err := svc.AddPrimary(hugrSrc); err != nil {
		return nil, err
	}
	if oidcSrc, ok := hugrSrc.(*oidcauth.Source); ok {
		svc.RegisterPromptLogin(oidcSrc.PromptLogin)
	}
	return svc, nil
}

// registerProviderAuth is Phase 7b: adds cfg.Auth provider entries
// (excluding the already-registered primary Source) onto the existing
// service. Entries with type=hugr become aliases onto the primary.
func registerProviderAuth(ctx context.Context, cfg *hugenruntime.RuntimeConfig, svc *auth.Service, logger *slog.Logger) error {
	primary := svc.Primary()
	specs := make([]auth.AuthSpec, 0, len(cfg.Auth))
	for _, a := range cfg.Auth {
		if a.Name == primary {
			// Primary Source is already registered; entries that want
			// to reuse it should declare a distinct name with type=hugr
			// (alias resolution handled by Service.BuildSources).
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
	return svc.BuildSources(ctx, specs, logger)
}
