package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	agentstore "github.com/hugr-lab/hugen/pkg/agent/store"
	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/identity"
	hugenruntime "github.com/hugr-lab/hugen/pkg/runtime"

	"github.com/hugr-lab/query-engine/client"
	qetypes "github.com/hugr-lab/query-engine/types"
)

// agentRuntime bundles all long-lived resources built at startup.
// Embeds *hugenruntime.Runtime for direct access to Agent / Sessions /
// Tools / Skills / Engine; shutdown delegates to Runtime.Close.
type agentRuntime struct {
	*hugenruntime.Runtime

	hugrClient *client.Client
}

func (r *agentRuntime) close(logger *slog.Logger) {
	if r == nil || r.Runtime == nil {
		return
	}
	logger.Info("shutting down: closing runtime")
	r.Close()
}

// buildAuthForBootstrap is Phase A+B.1: builds the single hugr Source
// declared by .env, wires it into a fresh SourceRegistry, mounts the
// shared /auth/callback dispatcher, and returns the RoundTripper the
// hugr client + engine should use.
func buildAuthForBootstrap(ctx context.Context, boot *config.BootstrapConfig, mux *http.ServeMux, logger *slog.Logger) (*auth.SourceRegistry, http.RoundTripper, error) {
	reg := auth.NewSourceRegistry(logger)

	hugrSrc, err := auth.BuildHugrSource(ctx, auth.AuthSpec{
		Name:        boot.HugrAuth.Name,
		Type:        boot.HugrAuth.Type,
		AccessToken: boot.HugrAuth.AccessToken,
		TokenURL:    boot.HugrAuth.TokenURL,
		Issuer:      boot.HugrAuth.Issuer,
		ClientID:    boot.HugrAuth.ClientID,
		BaseURL:     boot.A2A.BaseURL,
		DiscoverURL: boot.Hugr.URL,
	}, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("hugr source: %w", err)
	}
	if err := reg.AddPrimary(hugrSrc); err != nil {
		return nil, nil, err
	}
	if oidc, ok := hugrSrc.(*auth.OIDCStore); ok {
		reg.RegisterPromptLogin(oidc.PromptLogin)
	}
	reg.Mount(mux)

	return reg, auth.Transport(hugrSrc, http.DefaultTransport), nil
}

// loadFullConfig is Phase B.3: chooses between local YAML and remote
// hub pull based on boot.Remote(). In remote mode it also resolves
// agent_id via whoami before the GraphQL fetch.
func loadFullConfig(ctx context.Context, boot *config.BootstrapConfig, hugrClient *client.Client, logger *slog.Logger) (*config.Config, error) {
	if !boot.Remote() {
		return config.LoadLocal("config.yaml", boot)
	}
	who, err := identity.ResolveFromHugr(ctx, hugrClient)
	if err != nil {
		return nil, fmt.Errorf("remote identity: %w", err)
	}
	boot.Identity.ID = who.UserID
	boot.Identity.Name = who.UserName
	logger.Info("remote identity resolved", "agent_id", who.UserID, "name", who.UserName)

	cfg, err := config.LoadRemote(ctx, hugrClient, boot.Identity.ID, boot)
	if err != nil {
		return nil, fmt.Errorf("remote config: %w", err)
	}
	return cfg, nil
}

// buildRuntime is a thin wrapper around pkg/runtime.Build that stitches
// in bootstrap-owned externals (auth registry, remote hugr client) and
// runs selfRegisterAgent in fully-local mode. The actual wiring (engine,
// session manager, memory/chat subsystems, root agent) lives in
// pkg/runtime so the scenario harness can reuse the same assembly
// without auth/remote shim code.
func buildRuntime(
	ctx context.Context,
	boot *config.BootstrapConfig,
	cfg *config.Config,
	logger *slog.Logger,
	authReg *auth.SourceRegistry,
	hugrClient *client.Client,
) (*agentRuntime, error) {
	// Default HUGR_MCP_URL so inline endpoint specs in skills can still
	// reference ${HUGR_MCP_URL} if they want an anonymous MCP binding.
	if os.Getenv("HUGR_MCP_URL") == "" && cfg.Hugr.URL != "" {
		_ = os.Setenv("HUGR_MCP_URL", cfg.Hugr.URL+"/mcp")
	}

	opts := hugenruntime.Options{
		BaseTransport: http.DefaultTransport,
	}
	if authReg != nil {
		opts.AuthStores = authReg.TokenStores()
	}
	if hugrClient != nil {
		opts.HugrClient = hugrClient
		opts.HugrClientClose = hugrClient.CloseSubscriptions
	}

	rt, err := hugenruntime.Build(ctx, cfg, logger, opts)
	if err != nil {
		return nil, err
	}

	// Self-register runs only in fully local mode — hub owns the
	// agents row in every other combination.
	if !boot.Remote() && cfg.LocalDBEnabled {
		if err := selfRegisterAgent(ctx, cfg, rt.Querier, logger); err != nil {
			rt.Close()
			return nil, err
		}
	}

	return &agentRuntime{
		Runtime:    rt,
		hugrClient: hugrClient,
	}, nil
}

// selfRegisterAgent constructs a short-lived agentstore client and
// upserts the agents row. Runs only in fully-local mode — in every
// other combination the hub owns the registry entry.
func selfRegisterAgent(ctx context.Context, cfg *config.Config, querier qetypes.Querier, logger *slog.Logger) error {
	reg, err := agentstore.New(querier, agentstore.Options{
		AgentID: cfg.Identity.ID, AgentShort: cfg.Identity.ShortID, Logger: logger,
	})
	if err != nil {
		return fmt.Errorf("agent registry client: %w", err)
	}
	return registerAgentInstance(ctx, cfg, reg, logger)
}

// registerAgentInstance verifies the agent_type row exists (seeded at
// migration) and upserts the agents row with the current
// config_override. Runs only in local mode — in hub mode the hub owns
// registration.
func registerAgentInstance(ctx context.Context, cfg *config.Config, reg *agentstore.Client, logger *slog.Logger) error {
	at, err := reg.GetAgentType(ctx, cfg.Identity.Type)
	if err != nil {
		return fmt.Errorf("get agent_type %q: %w", cfg.Identity.Type, err)
	}
	if at == nil {
		return fmt.Errorf("agent type %q not found in hub.db — re-create memory.db", cfg.Identity.Type)
	}

	override := map[string]any{
		"llm":       cfg.LLM,
		"embedding": cfg.Embedding,
		"memory":    cfg.Memory,
	}
	if err := reg.RegisterAgent(ctx, agentstore.Agent{
		ID:             cfg.Identity.ID,
		AgentTypeID:    cfg.Identity.Type,
		ShortID:        cfg.Identity.ShortID,
		Name:           cfg.Identity.Name,
		ConfigOverride: override,
	}); err != nil {
		return fmt.Errorf("register agent %q: %w", cfg.Identity.ID, err)
	}
	logger.Info("agent registered", "id", cfg.Identity.ID, "type", cfg.Identity.Type)
	return nil
}
