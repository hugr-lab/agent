// Package config composes YAML-shaped sub-configs owned by their
// domain packages into a single Config loaded from .env + config.yaml.
//
// pkg/config does not own domain types — each sub-config (memory,
// chatcontext, skills, models, a2a, devui, agent, tools, store/local)
// is declared in its owner package and referenced here via
// composition. The only types still owned by pkg/config are
// cross-cutting (HugrConfig — platform connection, AuthConfig —
// pending separate refactor).
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"

	"github.com/hugr-lab/hugen/pkg/a2a"
	"github.com/hugr-lab/hugen/pkg/agent"
	"github.com/hugr-lab/hugen/pkg/chatcontext"
	"github.com/hugr-lab/hugen/pkg/devui"
	"github.com/hugr-lab/hugen/pkg/memory"
	"github.com/hugr-lab/hugen/pkg/models"
	"github.com/hugr-lab/hugen/pkg/skills"
	"github.com/hugr-lab/hugen/pkg/store/local"
	"github.com/hugr-lab/hugen/pkg/tools"
)

// Config is the application configuration: pure composition of
// domain-owned sub-configs.
type Config struct {
	Hugr           HugrConfig
	Identity       local.Identity
	Embedding      local.EmbeddingConfig
	LocalDBEnabled bool
	LocalDB        local.Config

	Agent       agent.Config
	Skills      skills.Config
	A2A         a2a.Config
	DevUI       devui.Config
	LLM         models.Config
	Memory      memory.Config
	ChatContext chatcontext.Config
	MCP         tools.MCPConfig

	Auth      []AuthConfig
	Providers []tools.ProviderConfig
}

// AuthConfig declares a named auth mechanism. Callers build a
// TokenStore per config and mount any required HTTP callback routes on
// the given mux. callback_path must be unique across all OIDC entries
// in the same process.
//
// Remains in pkg/config pending a separate auth-focused refactor.
type AuthConfig struct {
	Name         string `mapstructure:"name"`
	Type         string `mapstructure:"type"` // hugr | oidc
	Issuer       string `mapstructure:"issuer"`
	ClientID     string `mapstructure:"client_id"`
	CallbackPath string `mapstructure:"callback_path"`
	LoginPath    string `mapstructure:"login_path"`
	AccessToken  string `mapstructure:"access_token"`
	TokenURL     string `mapstructure:"token_url"`
}

// HugrConfig — platform connection. URL comes from .env:HUGR_URL (not
// YAML); MCPUrl is derived; Auth is a name reference into the Auth
// list used by the hugr LLM client + engine transport.
type HugrConfig struct {
	URL    string
	MCPUrl string
	Auth   string
}

func baseURL(configured string, port int) string {
	if configured != "" {
		return strings.TrimRight(configured, "/")
	}
	return fmt.Sprintf("http://localhost:%d", port)
}

// Load is the back-compat single-shot loader: it composes
// LoadBootstrap + LoadLocal and returns the full Config. New
// callers should use LoadBootstrap directly and then dispatch to
// LoadLocal or LoadRemote depending on boot.Remote().
func Load(yamlPath string) (*Config, error) {
	boot, err := LoadBootstrap(".env")
	if err != nil {
		return nil, err
	}
	return LoadLocal(yamlPath, boot)
}

// LoadLocal returns the full Config derived from .env env-defaults
// (carried in boot) plus the YAML file at yamlPath. Passing "" for
// yamlPath skips YAML loading (tests).
func LoadLocal(yamlPath string, boot *BootstrapConfig) (*Config, error) {
	if boot == nil {
		return nil, fmt.Errorf("config: LoadLocal requires BootstrapConfig")
	}
	v := viper.New()
	v.AutomaticEnv()
	v.SetDefault("AGENT_MODEL", "gemma4-26b")
	v.SetDefault("AGENT_CONSTITUTION", "constitution/base.md")
	v.SetDefault("AGENT_SKILLS_PATH", "./skills")
	v.SetDefault("AGENT_MAX_TOKENS", 0)

	cfg := &Config{
		Hugr:     boot.Hugr,
		A2A:      boot.A2A,
		DevUI:    boot.DevUI,
		Identity: boot.Identity,
		Agent: agent.Config{
			Constitution: v.GetString("AGENT_CONSTITUTION"),
		},
		Skills: skills.Config{
			Path: v.GetString("AGENT_SKILLS_PATH"),
		},
		LLM: models.Config{
			Model:     v.GetString("AGENT_MODEL"),
			MaxTokens: v.GetInt("AGENT_MAX_TOKENS"),
		},
	}

	if yamlPath != "" {
		if err := applyYAML(cfg, yamlPath); err != nil {
			return nil, fmt.Errorf("config: load yaml %s: %w", yamlPath, err)
		}
	}

	if cfg.MCP.TTL == 0 {
		cfg.MCP.TTL = 60 * time.Second
	}
	if cfg.MCP.FetchTimeout == 0 {
		cfg.MCP.FetchTimeout = 30 * time.Second
	}

	return cfg, nil
}

// expandAuthEnv replaces ${VAR} references with values from the
// process environment in every env-bearing AuthConfig field. Unset
// vars expand to "" — which, for type=hugr, drops us from token mode
// into OIDC discovery (the correct dev-default behaviour).
func expandAuthEnv(list []AuthConfig) {
	for i := range list {
		a := &list[i]
		a.Issuer = os.ExpandEnv(a.Issuer)
		a.ClientID = os.ExpandEnv(a.ClientID)
		a.AccessToken = os.ExpandEnv(a.AccessToken)
		a.TokenURL = os.ExpandEnv(a.TokenURL)
		a.CallbackPath = os.ExpandEnv(a.CallbackPath)
		a.LoginPath = os.ExpandEnv(a.LoginPath)
	}
}

// expandProvidersEnv does the same for tools.ProviderConfig fields
// that accept ${VAR} in yaml.
func expandProvidersEnv(list []tools.ProviderConfig) {
	for i := range list {
		p := &list[i]
		p.Endpoint = os.ExpandEnv(p.Endpoint)
		p.Command = os.ExpandEnv(p.Command)
		for j := range p.Args {
			p.Args[j] = os.ExpandEnv(p.Args[j])
		}
		for k, v := range p.Env {
			p.Env[k] = os.ExpandEnv(v)
		}
	}
}

// validateAuth enforces unique auth names + unique callback paths (for
// oidc entries). Called at load time — failures abort startup so we
// never get a silent route collision at first-request time.
func validateAuth(list []AuthConfig) error {
	seenNames := map[string]struct{}{}
	seenPaths := map[string]string{}
	for _, a := range list {
		if a.Name == "" {
			return fmt.Errorf("config: auth entry has empty name")
		}
		if _, dup := seenNames[a.Name]; dup {
			return fmt.Errorf("config: duplicate auth name %q", a.Name)
		}
		seenNames[a.Name] = struct{}{}

		switch a.Type {
		case "hugr", "oidc":
			// Both types may end up registering an OIDC callback —
			// `hugr` only when it falls back to OIDC discovery. We
			// reserve the path at config-parse time so two auth
			// entries can't race for it at runtime.
			path := a.CallbackPath
			if path == "" {
				path = "/auth/callback"
			}
			if owner, dup := seenPaths[path]; dup {
				return fmt.Errorf("config: auth %q callback_path %q collides with auth %q", a.Name, path, owner)
			}
			seenPaths[path] = a.Name
		default:
			return fmt.Errorf("config: auth %q has unsupported type %q (want hugr|oidc)", a.Name, a.Type)
		}
	}
	return nil
}

// validateProviders enforces unique provider names and that every
// provider's `auth:` reference (when set) exists in the auth list.
func validateProviders(list []tools.ProviderConfig, auths []AuthConfig) error {
	seen := map[string]struct{}{}
	authNames := map[string]struct{}{}
	for _, a := range auths {
		authNames[a.Name] = struct{}{}
	}
	for _, p := range list {
		if p.Name == "" {
			return fmt.Errorf("config: provider has empty name")
		}
		if _, dup := seen[p.Name]; dup {
			return fmt.Errorf("config: duplicate provider name %q", p.Name)
		}
		seen[p.Name] = struct{}{}
		if p.Type == "" {
			return fmt.Errorf("config: provider %q has empty type", p.Name)
		}
		if p.Auth != "" {
			if _, ok := authNames[p.Auth]; !ok {
				return fmt.Errorf("config: provider %q references unknown auth %q", p.Name, p.Auth)
			}
		}
	}
	return nil
}

// applyYAML unmarshals config.yaml into cfg, overwriting relevant
// sub-configs. The `agent:` key is read twice: once for Identity
// (id/short_id/name/type) and once for the Agent YAML section
// (constitution); unmarshal-by-tag does not conflict.
func applyYAML(cfg *Config, path string) error {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil // YAML is optional
		}
		return err
	}

	y := viper.New()
	y.SetConfigFile(path)
	y.SetConfigType("yaml")
	if err := y.ReadInConfig(); err != nil {
		return err
	}

	if err := unmarshalSections(y, cfg); err != nil {
		return err
	}
	expandAuthEnv(cfg.Auth)
	expandProvidersEnv(cfg.Providers)
	if err := validateAuth(cfg.Auth); err != nil {
		return err
	}
	if err := validateProviders(cfg.Providers, cfg.Auth); err != nil {
		return err
	}
	return nil
}
