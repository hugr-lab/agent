package config

import (
	"context"
	"fmt"
	"time"

	"github.com/hugr-lab/query-engine/types"
	"github.com/spf13/viper"

	agentstore "github.com/hugr-lab/hugen/pkg/agent/store"
)

// LoadRemote pulls the agent's config from hub.db.agents (merging
// agent_types.config defaults with agents.config_override) and
// returns a typed *Config. The bootstrap already has hugr + a2a +
// devui fields filled in from .env; those take precedence over
// whatever the hub returned for the same keys.
//
// Fails loudly if the agent row is missing — in remote mode hub
// is the authoritative registry, so the absence of a row signals
// misconfiguration rather than a condition we should paper over.
func LoadRemote(ctx context.Context, q types.Querier, agentID string, boot *BootstrapConfig) (*Config, error) {
	if boot == nil {
		return nil, fmt.Errorf("config: LoadRemote requires BootstrapConfig")
	}
	if q == nil {
		return nil, fmt.Errorf("config: LoadRemote requires a querier")
	}
	if agentID == "" {
		return nil, fmt.Errorf("config: LoadRemote requires non-empty agentID")
	}

	merged, agentRow, err := agentstore.LoadConfigFromHub(ctx, q, agentID)
	if err != nil {
		return nil, err
	}

	// Feed the merged map through viper to reuse the existing
	// per-section UnmarshalKey logic and env expansion. That keeps
	// all shape/validation semantics identical between YAML and
	// hub-sourced configs.
	v := viper.New()
	if err := v.MergeConfigMap(merged); err != nil {
		return nil, fmt.Errorf("config: merge remote config map: %w", err)
	}

	cfg := &Config{
		Hugr:  boot.Hugr,
		A2A:   boot.A2A,
		DevUI: boot.DevUI,
		Identity: boot.Identity,
	}

	// Same unmarshal sequence applyYAML uses, driven by viper.
	if err := unmarshalSections(v, cfg); err != nil {
		return nil, fmt.Errorf("config: decode remote: %w", err)
	}

	// Identity fields that came from agents row — they're not part
	// of the config shape but needed for the runtime.
	if cfg.Identity.ID == "" {
		cfg.Identity.ID = agentRow.ID
	}
	if cfg.Identity.ShortID == "" {
		cfg.Identity.ShortID = agentRow.ShortID
	}
	if cfg.Identity.Name == "" {
		cfg.Identity.Name = agentRow.Name
	}
	if cfg.Identity.Type == "" {
		cfg.Identity.Type = agentRow.AgentTypeID
	}

	expandAuthEnv(cfg.Auth)
	expandProvidersEnv(cfg.Providers)
	if err := validateAuth(cfg.Auth); err != nil {
		return nil, err
	}
	if err := validateProviders(cfg.Providers, cfg.Auth); err != nil {
		return nil, err
	}

	if cfg.MCP.TTL == 0 {
		cfg.MCP.TTL = 60 * time.Second
	}
	if cfg.MCP.FetchTimeout == 0 {
		cfg.MCP.FetchTimeout = 30 * time.Second
	}
	return cfg, nil
}

// unmarshalSections factors out the per-section UnmarshalKey chain
// shared by applyYAML and LoadRemote. Kept here (rather than in
// config.go) to keep the remote entry point self-contained.
func unmarshalSections(v *viper.Viper, cfg *Config) error {
	// agent: identity + constitution, read from the same key.
	if err := v.UnmarshalKey("agent", &cfg.Identity); err != nil {
		return fmt.Errorf("unmarshal agent (identity): %w", err)
	}
	if c := v.GetString("agent.constitution"); c != "" {
		cfg.Agent.Constitution = c
	}

	if err := v.UnmarshalKey("skills", &cfg.Skills); err != nil {
		return fmt.Errorf("unmarshal skills: %w", err)
	}
	if err := v.UnmarshalKey("a2a", &cfg.A2A); err != nil {
		return fmt.Errorf("unmarshal a2a: %w", err)
	}
	if err := v.UnmarshalKey("devui", &cfg.DevUI); err != nil {
		return fmt.Errorf("unmarshal devui: %w", err)
	}

	cfg.LocalDBEnabled = v.GetBool("local_db_enabled")
	if err := v.UnmarshalKey("local_db", &cfg.LocalDB); err != nil {
		return fmt.Errorf("unmarshal local_db: %w", err)
	}

	if err := v.UnmarshalKey("memory", &cfg.Memory); err != nil {
		return fmt.Errorf("unmarshal memory: %w", err)
	}
	if err := v.UnmarshalKey("chatcontext", &cfg.ChatContext); err != nil {
		return fmt.Errorf("unmarshal chatcontext: %w", err)
	}
	if err := v.UnmarshalKey("llm", &cfg.LLM); err != nil {
		return fmt.Errorf("unmarshal llm: %w", err)
	}
	if err := v.UnmarshalKey("embedding", &cfg.Embedding); err != nil {
		return fmt.Errorf("unmarshal embedding: %w", err)
	}
	if err := v.UnmarshalKey("mcp", &cfg.MCP); err != nil {
		return fmt.Errorf("unmarshal mcp: %w", err)
	}

	if a := v.GetString("hugr.auth"); a != "" {
		cfg.Hugr.Auth = a
	}
	if err := v.UnmarshalKey("auth", &cfg.Auth); err != nil {
		return fmt.Errorf("unmarshal auth: %w", err)
	}
	if err := v.UnmarshalKey("providers", &cfg.Providers); err != nil {
		return fmt.Errorf("unmarshal providers: %w", err)
	}
	return nil
}
