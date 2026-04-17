// Package config provides application configuration loaded from environment and config.yaml.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config holds all application configuration.
type Config struct {
	Hugr      HugrConfig
	Agent     AgentConfig
	Identity  AgentIdentity
	HugrLocal HugrLocalConfig
	Memory    MemoryConfig
	LLM       LLMConfig
	Embedding EmbeddingConfig
	Models    []ModelDef
}

// ── Hugr server connection ───────────────────────────────────

// HugrConfig holds Hugr server connection settings.
type HugrConfig struct {
	URL       string
	MCPUrl    string
	SecretKey string

	AccessToken string
	TokenURL    string

	OIDCIssuer   string
	OIDCClientID string
}

func (h HugrConfig) UseTokenAuth() bool {
	return h.AccessToken != "" && h.TokenURL != ""
}

func (h HugrConfig) UseOIDC() bool {
	return h.OIDCIssuer != "" && h.OIDCClientID != ""
}

func (h HugrConfig) CanDiscoverOIDC() bool {
	return h.OIDCIssuer == "" && h.OIDCClientID == "" && h.URL != ""
}

// ── Agent runtime (legacy — LLM model name + skills path + server) ────

// AgentConfig holds agent runtime settings.
type AgentConfig struct {
	Model        string
	Constitution string
	SkillsPath   string
	MaxTokens    int
	Temperature  float32
	Port         int
	BaseURL      string
}

// ── Agent identity (from config.yaml agent: ...) ────────────

// AgentIdentity uniquely identifies a running agent instance.
type AgentIdentity struct {
	ID      string `mapstructure:"id"`
	ShortID string `mapstructure:"short_id"`
	Name    string `mapstructure:"name"`
	Type    string `mapstructure:"type"`
}

// ── Embedded hugr engine ────────────────────────────────────

type HugrLocalConfig struct {
	Enabled bool        `mapstructure:"enabled"`
	DB      HugrLocalDB `mapstructure:"db"`
}

type HugrLocalDB struct {
	Path     string         `mapstructure:"path"`
	Settings HugrLocalDBSet `mapstructure:"settings"`
}

type HugrLocalDBSet struct {
	MaxMemory     int    `mapstructure:"max_memory"`
	WorkerThreads int    `mapstructure:"worker_threads"`
	HomeDirectory string `mapstructure:"home_directory"`
	Timezone      string `mapstructure:"timezone"`
}

// ── Memory storage ──────────────────────────────────────────

type MemoryConfig struct {
	Mode                string                   `mapstructure:"mode"`
	Path                string                   `mapstructure:"path"`
	HugrURL             string                   `mapstructure:"hugr_url"`
	VolatilityDuration  map[string]time.Duration `mapstructure:"volatility_duration"`
	CompactionThreshold float64                  `mapstructure:"compaction_threshold"`
	Scheduler           MemoryScheduler          `mapstructure:"scheduler"`
	Consolidation       MemoryConsolidation      `mapstructure:"consolidation"`
}

type MemoryScheduler struct {
	Interval    time.Duration `mapstructure:"interval"`
	ReviewDelay time.Duration `mapstructure:"review_delay"`
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

// ── Embedding ───────────────────────────────────────────────

type EmbeddingConfig struct {
	Mode      string `mapstructure:"mode"`
	Model     string `mapstructure:"model"`
	Dimension int    `mapstructure:"dimension"`
}

// ── Data source definitions (models[]) ──────────────────────

// ModelDef registers a single LLM/embedding data source in the local engine.
type ModelDef struct {
	Name string `mapstructure:"name"`
	Type string `mapstructure:"type"` // llm-openai | llm-anthropic | llm-gemini | embedding
	Path string `mapstructure:"path"` // URL + query params; ${ENV_VAR} expanded at attach time
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

	_ = v.ReadInConfig()

	hugrURL := strings.TrimRight(v.GetString("HUGR_URL"), "/")
	port := v.GetInt("AGENT_PORT")

	cfg := &Config{
		Hugr: HugrConfig{
			URL:          hugrURL,
			MCPUrl:       hugrURL + "/mcp",
			SecretKey:    v.GetString("HUGR_SECRET_KEY"),
			AccessToken:  v.GetString("HUGR_ACCESS_TOKEN"),
			TokenURL:     v.GetString("HUGR_TOKEN_URL"),
			OIDCIssuer:   v.GetString("HUGR_OIDC_ISSUER"),
			OIDCClientID: v.GetString("HUGR_OIDC_CLIENT_ID"),
		},
		Agent: AgentConfig{
			Model:        v.GetString("AGENT_MODEL"),
			Constitution: v.GetString("AGENT_CONSTITUTION"),
			SkillsPath:   v.GetString("AGENT_SKILLS_PATH"),
			MaxTokens:    v.GetInt("AGENT_MAX_TOKENS"),
			Port:         port,
			BaseURL:      baseURL(v.GetString("AGENT_BASE_URL"), port),
		},
	}

	if yamlPath != "" {
		if err := applyYAML(cfg, yamlPath); err != nil {
			return nil, fmt.Errorf("config: load yaml %s: %w", yamlPath, err)
		}
	}

	return cfg, nil
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

	// Embedded engine
	if err := y.UnmarshalKey("hugr_local", &cfg.HugrLocal); err != nil {
		return fmt.Errorf("unmarshal hugr_local: %w", err)
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

	// Models
	if err := y.UnmarshalKey("models", &cfg.Models); err != nil {
		return fmt.Errorf("unmarshal models: %w", err)
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
