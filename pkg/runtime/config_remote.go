package runtime

import (
	"context"
	"fmt"

	"github.com/spf13/viper"

	"github.com/hugr-lab/hugen/pkg/identity"
)

// LoadRemote pulls the agent's config via the supplied identity.Source
// (typically pkg/identity/hub) and returns a typed *RuntimeConfig. The
// bootstrap already has hugr + a2a + devui fields filled in from
// .env; those take precedence over whatever the hub returned for the
// same keys.
//
// Fails loudly if the agents row is missing — in remote mode hub
// is the authoritative registry, so the absence of a row signals
// misconfiguration rather than a condition we should paper over.
func LoadRemote(ctx context.Context, src identity.Source, boot *BootstrapConfig) (*RuntimeConfig, error) {
	if boot == nil {
		return nil, fmt.Errorf("config: LoadRemote requires BootstrapConfig")
	}
	if src == nil {
		return nil, fmt.Errorf("config: LoadRemote requires an identity.Source")
	}

	agent, err := src.Agent(ctx)
	if err != nil {
		return nil, err
	}

	// Feed the merged map through viper to reuse the same
	// per-section UnmarshalKey + env-expand + validate pipeline the
	// YAML path uses.
	v := viper.New()
	if err := v.MergeConfigMap(agent.Config); err != nil {
		return nil, fmt.Errorf("config: merge remote config map: %w", err)
	}

	cfg := &RuntimeConfig{
		Hugr:  boot.Hugr,
		A2A:   boot.A2A,
		DevUI: boot.DevUI,
	}
	if err := decodeAndFinalize(v, cfg); err != nil {
		return nil, fmt.Errorf("config: decode remote: %w", err)
	}

	// Identity fields that came from the agents row — they're not
	// part of the config shape but needed for the runtime.
	if cfg.Identity.ID == "" {
		cfg.Identity.ID = agent.ID
	}
	if cfg.Identity.ShortID == "" {
		cfg.Identity.ShortID = agent.ShortID
	}
	if cfg.Identity.Name == "" {
		cfg.Identity.Name = agent.Name
	}
	if cfg.Identity.Type == "" {
		cfg.Identity.Type = agent.AgentTypeID
	}
	return cfg, nil
}
