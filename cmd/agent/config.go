package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/hugr-lab/hugen/pkg/identity"
	hugenruntime "github.com/hugr-lab/hugen/pkg/runtime"
)

// buildRuntimeConfig is Phase 6: chooses between local YAML and
// remote hub pull based on boot.Remote(). Both paths funnel through
// the identity.Source so cfg.Identity gets populated consistently:
//
//   - remote: hub returns merged agent_type.config + agent.config_override
//     (plus the agents row's id/short_id/name/type).
//   - local : yaml file at "config.yaml" provides everything; identity
//     comes from the yaml's `agent:` block via local.Source.
func buildRuntimeConfig(ctx context.Context, boot *hugenruntime.BootstrapConfig, src identity.Source, logger *slog.Logger) (*hugenruntime.RuntimeConfig, error) {
	if boot.Remote() {
		who, err := src.WhoAmI(ctx)
		if err != nil {
			return nil, fmt.Errorf("remote identity: %w", err)
		}
		logger.Info("remote identity resolved", "agent_id", who.UserID, "name", who.UserName)
		cfg, err := hugenruntime.LoadRemote(ctx, src, boot)
		if err != nil {
			return nil, fmt.Errorf("remote config: %w", err)
		}
		return cfg, nil
	}

	cfg, err := hugenruntime.LoadLocal("config.yaml", boot, src)
	if err != nil {
		return nil, fmt.Errorf("local config: %w", err)
	}

	// Default HUGR_MCP_URL so inline endpoint specs in skills can
	// still reference ${HUGR_MCP_URL} if they want an anonymous MCP
	// binding. Only set when the operator hasn't already pinned it.
	if os.Getenv("HUGR_MCP_URL") == "" && cfg.Hugr.URL != "" {
		_ = os.Setenv("HUGR_MCP_URL", cfg.Hugr.URL+"/mcp")
	}
	return cfg, nil
}
