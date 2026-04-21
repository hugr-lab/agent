// Package config provides application configuration loaded from environment and config.yaml.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"

	"github.com/hugr-lab/hugen/pkg/store/local"
)

// Config holds all application configuration.
//
// Identity and Embedding reuse types from pkg/store/local — defining
// them there keeps the composition (Config → local.Config via
// LocalDB) cycle-free. Semantically they are global (used by both
// local and remote modes); the physical location is a cycle-
// avoidance detail.
type Config struct {
	Hugr           HugrConfig
	Agent          AgentConfig
	Identity       local.Identity
	Embedding      local.EmbeddingConfig
	LocalDBEnabled bool
	LocalDB        local.Config
	Memory         MemoryConfig
	LLM            LLMConfig
	MCP            MCPConfig
	Auth           []AuthConfig
	Providers      []ProviderConfig
}

// AuthConfig declares a named auth mechanism. Callers build a
// TokenStore per config and mount any required HTTP callback routes on
// the given mux. callback_path must be unique across all OIDC entries
// in the same process.
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

// ProviderConfig declares a tools.Provider instance built at startup.
// The `type` picks a builder registered in pkg/providers; the rest of
// the fields are type-specific.
//
// For type=mcp the `transport` field selects the MCP transport:
//   - "streamable-http" (default): Streamable HTTP (one URL, one HTTP
//     client, bidirectional chunked JSON-RPC).
//   - "sse": Server-Sent Events over HTTP.
//   - "stdio": spawns `command` + `args` and talks JSON-RPC over
//     stdin/stdout. Auth-by-name is ignored; credentials go through
//     env instead.
type ProviderConfig struct {
	Name      string            `mapstructure:"name"`
	Type      string            `mapstructure:"type"`      // mcp | system
	Suite     string            `mapstructure:"suite"`     // for type=system: skills|memory|...
	Transport string            `mapstructure:"transport"` // for type=mcp: streamable-http|sse|stdio
	Endpoint  string            `mapstructure:"endpoint"`  // for HTTP transports
	Command   string            `mapstructure:"command"`   // for stdio
	Args      []string          `mapstructure:"args"`      // for stdio
	Env       map[string]string `mapstructure:"env"`       // for stdio
	Auth      string            `mapstructure:"auth"`      // optional auth config name (HTTP only)
}

// MCPConfig controls behaviour of every MCP-backed tools.Provider.
type MCPConfig struct {
	// TTL is how long a provider keeps the ListTools result cached between
	// explicit invalidation events. Zero = no TTL (always refetch).
	TTL time.Duration `mapstructure:"ttl"`
	// FetchTimeout bounds a single ListTools call.
	FetchTimeout time.Duration `mapstructure:"fetch_timeout"`
}

// ── Hugr server connection ───────────────────────────────────

// HugrConfig holds Hugr server connection settings. Auth moved into
// top-level `auth:` list as of 005b — this block only carries the base
// URL, the MCP URL default, and a reference to which auth entry to use
// for the hugr LLM client + engine connection.
type HugrConfig struct {
	URL    string
	MCPUrl string
	Auth   string // name of an entry in cfg.Auth; empty = unauthenticated
}

// ── Agent runtime (legacy — LLM model name + skills path + server) ────

// AgentConfig holds agent runtime settings.
type AgentConfig struct {
	Model        string
	Constitution string
	SkillsPath   string
	MaxTokens    int
	Temperature  float32
	Port         int // A2A listener (and auth callbacks) — default 10000
	BaseURL      string
	// DevUIPort is the separate listener for the ADK devui + REST +
	// /dev/* helpers. Only consumed in `devui` mode; A2A and auth
	// callbacks stay on Port so redirect_uri configuration is
	// independent of the run mode.
	DevUIPort    int
	DevUIBaseURL string
}

// ── Memory storage ──────────────────────────────────────────

// MemoryConfig holds runtime memory-management tuning. The hub.db
// file path has moved into local.Config.MemoryPath (populated via
// LocalDB) since it is only relevant when the embedded engine runs.
type MemoryConfig struct {
	Mode                string                   `mapstructure:"mode"`
	HugrURL             string                   `mapstructure:"hugr_url"`
	VolatilityDuration  map[string]time.Duration `mapstructure:"volatility_duration"`
	CompactionThreshold float64                  `mapstructure:"compaction_threshold"`
	Scheduler           MemoryScheduler          `mapstructure:"scheduler"`
	Consolidation       MemoryConsolidation      `mapstructure:"consolidation"`
}

type MemoryScheduler struct {
	Interval        time.Duration `mapstructure:"interval"`
	ReviewDelay     time.Duration `mapstructure:"review_delay"`
	ConsolidationAt string        `mapstructure:"consolidation_at"`
}

type MemoryConsolidation struct {
	HypothesisExpiry time.Duration `mapstructure:"hypothesis_expiry"`
}

// ── LLM routing ─────────────────────────────────────────────

type LLMConfig struct {
	Mode        string            `mapstructure:"mode"`
	Model       string            `mapstructure:"model"`
	Temperature float32           `mapstructure:"temperature"`
	MaxTokens   int               `mapstructure:"max_tokens"`
	Routes      map[string]string `mapstructure:"routes"`
}

// ── Loading ─────────────────────────────────────────────────

func baseURL(configured string, port int) string {
	if configured != "" {
		return strings.TrimRight(configured, "/")
	}
	return fmt.Sprintf("http://localhost:%d", port)
}

// Load reads configuration from .env, environment variables, and config.yaml.
// yamlPath may be empty to skip YAML loading.
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
		Agent: AgentConfig{
			Model:        v.GetString("AGENT_MODEL"),
			Constitution: v.GetString("AGENT_CONSTITUTION"),
			SkillsPath:   v.GetString("AGENT_SKILLS_PATH"),
			MaxTokens:    v.GetInt("AGENT_MAX_TOKENS"),
			Port:         port,
			BaseURL:      baseURL(v.GetString("AGENT_BASE_URL"), port),
			DevUIPort:    devUIPort,
			DevUIBaseURL: baseURL(v.GetString("AGENT_DEVUI_BASE_URL"), devUIPort),
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

// expandProvidersEnv does the same for ProviderConfig fields that
// accept ${VAR} in yaml.
func expandProvidersEnv(list []ProviderConfig) {
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
func validateProviders(list []ProviderConfig, auths []AuthConfig) error {
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

// applyYAML unmarshals config.yaml into cfg, overwriting relevant fields.
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

	// Agent identity
	if err := y.UnmarshalKey("agent", &cfg.Identity); err != nil {
		return fmt.Errorf("unmarshal agent: %w", err)
	}

	// Local DB gate + section
	cfg.LocalDBEnabled = y.GetBool("local_db_enabled")
	if err := y.UnmarshalKey("local_db", &cfg.LocalDB); err != nil {
		return fmt.Errorf("unmarshal local_db: %w", err)
	}

	// Memory
	if err := y.UnmarshalKey("memory", &cfg.Memory); err != nil {
		return fmt.Errorf("unmarshal memory: %w", err)
	}

	// LLM
	if err := y.UnmarshalKey("llm", &cfg.LLM); err != nil {
		return fmt.Errorf("unmarshal llm: %w", err)
	}

	// Embedding
	if err := y.UnmarshalKey("embedding", &cfg.Embedding); err != nil {
		return fmt.Errorf("unmarshal embedding: %w", err)
	}

	// MCP (optional; defaults applied below).
	if err := y.UnmarshalKey("mcp", &cfg.MCP); err != nil {
		return fmt.Errorf("unmarshal mcp: %w", err)
	}

	// Hugr block can set the auth name used for the LLM client / engine
	// transport. URL already came from env; auth only lives in yaml.
	if a := y.GetString("hugr.auth"); a != "" {
		cfg.Hugr.Auth = a
	}

	// Auth + providers (new in 005b — skills-driven architecture).
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

	// Legacy AgentConfig fields from YAML (llm.model, llm.max_tokens, skills.path) —
	// keep behaviour consistent with what buildAgent used to do itself.
	if cfg.LLM.Model != "" {
		cfg.Agent.Model = cfg.LLM.Model
	}
	if cfg.LLM.MaxTokens > 0 {
		cfg.Agent.MaxTokens = cfg.LLM.MaxTokens
	}
	if cfg.LLM.Temperature > 0 {
		cfg.Agent.Temperature = cfg.LLM.Temperature
	}
	if sp := y.GetString("skills.path"); sp != "" {
		cfg.Agent.SkillsPath = sp
	}

	return nil
}
