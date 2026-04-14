// Package config provides application configuration loaded from environment.
package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/viper"
)

// Config holds all application configuration.
type Config struct {
	Hugr  HugrConfig
	Agent AgentConfig
}

// HugrConfig holds Hugr server connection settings.
type HugrConfig struct {
	URL       string // Hugr server base URL
	MCPUrl    string // MCP endpoint URL (derived from URL)
	SecretKey string // Dev-only: secret key auth (bypasses token auth)

	// Token-based auth (production).
	AccessToken string // Initial Bearer access token
	TokenURL    string // Token exchange service URL

	// OIDC auth (dev).
	OIDCIssuer   string // OIDC provider issuer URL
	OIDCClientID string // OIDC public client ID
}

// UseTokenAuth returns true when token-based auth is configured.
func (h HugrConfig) UseTokenAuth() bool {
	return h.AccessToken != "" && h.TokenURL != ""
}

// UseOIDC returns true when OIDC browser flow can be used.
// Either explicit config or auto-discovery from Hugr.
func (h HugrConfig) UseOIDC() bool {
	return h.OIDCIssuer != "" && h.OIDCClientID != ""
}

// CanDiscoverOIDC returns true when we should try to fetch OIDC config from Hugr.
func (h HugrConfig) CanDiscoverOIDC() bool {
	return h.OIDCIssuer == "" && h.OIDCClientID == "" && h.URL != ""
}

func baseURL(configured string, port int) string {
	if configured != "" {
		return strings.TrimRight(configured, "/")
	}
	return fmt.Sprintf("http://localhost:%d", port)
}


// AgentConfig holds agent runtime settings.
type AgentConfig struct {
	Model        string // Hugr LLM data source name
	Constitution string // Path to system prompt file
	SkillsPath   string // Directory containing skill packages
	MaxTokens    int     // Max completion tokens per LLM call (0 = provider default)
	Temperature  float32 // Default temperature (0 = provider default)
	Port         int    // Web server port
	BaseURL      string // Public base URL (e.g. https://agent.example.com)
}

// Load reads configuration from .env file and environment variables.
func Load() (*Config, error) {
	v := viper.New()
	v.SetConfigFile(".env")
	v.SetConfigType("env")
	v.AutomaticEnv()

	// Defaults
	v.SetDefault("HUGR_URL", "http://localhost:15000")
	v.SetDefault("AGENT_MODEL", "gemma4-26b")
	v.SetDefault("AGENT_CONSTITUTION", "constitution/base.md")
	v.SetDefault("AGENT_SKILLS_PATH", "./skills")
	v.SetDefault("AGENT_MAX_TOKENS", 0)
	v.SetDefault("AGENT_PORT", 10000)

	// Read .env file (optional — env vars take precedence)
	_ = v.ReadInConfig()

	hugrURL := strings.TrimRight(v.GetString("HUGR_URL"), "/")

	// Default HUGR_MCP_URL so skills can reference it via ${HUGR_MCP_URL}.
	if os.Getenv("HUGR_MCP_URL") == "" {
		os.Setenv("HUGR_MCP_URL", hugrURL+"/mcp")
	}
	port := v.GetInt("AGENT_PORT")

	return &Config{
		Hugr: HugrConfig{
			URL:         hugrURL,
			MCPUrl:      hugrURL + "/mcp",
			SecretKey:   v.GetString("HUGR_SECRET_KEY"),
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
	}, nil
}
