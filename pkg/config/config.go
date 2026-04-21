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

	agentcfg "github.com/hugr-lab/hugen/pkg/agent"
	a2acfg "github.com/hugr-lab/hugen/pkg/a2a"
	chatcontextcfg "github.com/hugr-lab/hugen/pkg/chatcontext"
	devuicfg "github.com/hugr-lab/hugen/pkg/devui"
	memorycfg "github.com/hugr-lab/hugen/pkg/memory"
	modelscfg "github.com/hugr-lab/hugen/pkg/models"
	skillscfg "github.com/hugr-lab/hugen/pkg/skills"
	"github.com/hugr-lab/hugen/pkg/store/local"
	toolscfg "github.com/hugr-lab/hugen/pkg/tools"
)

// Config is the application configuration: pure composition of
// domain-owned sub-configs.
type Config struct {
	Hugr           HugrConfig
	Identity       local.Identity
	Embedding      local.EmbeddingConfig
	LocalDBEnabled bool
	LocalDB        local.Config

	Agent       agentcfg.Config
	Skills      skillscfg.Config
	A2A         a2acfg.Config
	DevUI       devuicfg.Config
	LLM         modelscfg.Config
	Memory      memorycfg.Config
	ChatContext chatcontextcfg.Config
	MCP         toolscfg.MCPConfig

	Auth      []AuthConfig
	Providers []toolscfg.ProviderConfig
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

// Load reads configuration from .env, environment variables, and
// config.yaml. yamlPath may be empty to skip YAML loading.
func Load(yamlPath string) (*Config, error) {
	v := viper.New()
	v.SetConfigFile(".env")
	v.SetConfigType("env")
	v.AutomaticEnv()

	v.SetDefault("HUGR_URL", "http://localhost:15000")
	v.SetDefault("AGENT_MODEL", "gemma4-26b")
	v.SetDefault("AGENT_CONSTITUTION", "constitution/base.md")
	v.SetDefault("AGENT_SKILLS_PATH", "./skills")
	v.SetDefault("AGENT_MAX_TOKENS", 0)
	v.SetDefault("AGENT_PORT", 10000)
	v.SetDefault("AGENT_DEVUI_PORT", 10001)

	_ = v.ReadInConfig()

	// Propagate .env values into the process environment so os.ExpandEnv
	// in config.yaml paths (${API_KEY}, ${LLM_URL}, …) resolves them.
	for _, key := range v.AllKeys() {
		upper := strings.ToUpper(key)
		if _, set := os.LookupEnv(upper); set {
			continue
		}
		os.Setenv(upper, v.GetString(key))
	}

	hugrURL := strings.TrimRight(v.GetString("HUGR_URL"), "/")
	port := v.GetInt("AGENT_PORT")
	devUIPort := v.GetInt("AGENT_DEVUI_PORT")

	// Default HUGR_MCP_URL before yaml expansion so ${HUGR_MCP_URL}
	// in providers:/auth: resolves even when the operator didn't set
	// it explicitly in .env.
	if os.Getenv("HUGR_MCP_URL") == "" && hugrURL != "" {
		os.Setenv("HUGR_MCP_URL", hugrURL+"/mcp")
	}

	cfg := &Config{
		Hugr: HugrConfig{
			URL:    hugrURL,
			MCPUrl: hugrURL + "/mcp",
		},
		Agent: agentcfg.Config{
			Constitution: v.GetString("AGENT_CONSTITUTION"),
		},
		Skills: skillscfg.Config{
			Path: v.GetString("AGENT_SKILLS_PATH"),
		},
		LLM: modelscfg.Config{
			Model:     v.GetString("AGENT_MODEL"),
			MaxTokens: v.GetInt("AGENT_MAX_TOKENS"),
		},
		A2A: a2acfg.Config{
			Port:    port,
			BaseURL: baseURL(v.GetString("AGENT_BASE_URL"), port),
		},
		DevUI: devuicfg.Config{
			Port:    devUIPort,
			BaseURL: baseURL(v.GetString("AGENT_DEVUI_BASE_URL"), devUIPort),
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
func expandProvidersEnv(list []toolscfg.ProviderConfig) {
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
func validateProviders(list []toolscfg.ProviderConfig, auths []AuthConfig) error {
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

	// agent: contains both Identity (id/short_id/name/type) and
	// agent.Config (constitution). Read both from the same key.
	if err := y.UnmarshalKey("agent", &cfg.Identity); err != nil {
		return fmt.Errorf("unmarshal agent (identity): %w", err)
	}
	if c := y.GetString("agent.constitution"); c != "" {
		cfg.Agent.Constitution = c
	}

	if err := y.UnmarshalKey("skills", &cfg.Skills); err != nil {
		return fmt.Errorf("unmarshal skills: %w", err)
	}

	if err := y.UnmarshalKey("a2a", &cfg.A2A); err != nil {
		return fmt.Errorf("unmarshal a2a: %w", err)
	}
	if err := y.UnmarshalKey("devui", &cfg.DevUI); err != nil {
		return fmt.Errorf("unmarshal devui: %w", err)
	}

	// Local DB gate + section
	cfg.LocalDBEnabled = y.GetBool("local_db_enabled")
	if err := y.UnmarshalKey("local_db", &cfg.LocalDB); err != nil {
		return fmt.Errorf("unmarshal local_db: %w", err)
	}

	// Memory (YAML schema: volatility_duration / consolidation /
	// scheduler — mode/hugr_url/compaction_threshold removed).
	if err := y.UnmarshalKey("memory", &cfg.Memory); err != nil {
		return fmt.Errorf("unmarshal memory: %w", err)
	}

	// Chatcontext (own block — compaction threshold).
	if err := y.UnmarshalKey("chatcontext", &cfg.ChatContext); err != nil {
		return fmt.Errorf("unmarshal chatcontext: %w", err)
	}

	// LLM routing + defaults.
	if err := y.UnmarshalKey("llm", &cfg.LLM); err != nil {
		return fmt.Errorf("unmarshal llm: %w", err)
	}

	// Embedding (top-level).
	if err := y.UnmarshalKey("embedding", &cfg.Embedding); err != nil {
		return fmt.Errorf("unmarshal embedding: %w", err)
	}

	if err := y.UnmarshalKey("mcp", &cfg.MCP); err != nil {
		return fmt.Errorf("unmarshal mcp: %w", err)
	}

	// Hugr block auth-ref (URL stays from env).
	if a := y.GetString("hugr.auth"); a != "" {
		cfg.Hugr.Auth = a
	}

	if err := y.UnmarshalKey("auth", &cfg.Auth); err != nil {
		return fmt.Errorf("unmarshal auth: %w", err)
	}
	if err := y.UnmarshalKey("providers", &cfg.Providers); err != nil {
		return fmt.Errorf("unmarshal providers: %w", err)
	}
	expandAuthEnv(cfg.Auth)
	expandProvidersEnv(cfg.Providers)
	if err := validateAuth(cfg.Auth); err != nil {
		return err
	}
	if err := validateProviders(cfg.Providers, cfg.Auth); err != nil {
		return err
	}
	if cfg.MCP.TTL == 0 {
		cfg.MCP.TTL = 60 * time.Second
	}
	if cfg.MCP.FetchTimeout == 0 {
		cfg.MCP.FetchTimeout = 30 * time.Second
	}

	return nil
}
