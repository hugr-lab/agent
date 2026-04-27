package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// LoadBootstrap leans on viper.AutomaticEnv + process env. We scope
// env changes with t.Setenv so tests don't leak into each other.

func TestLoadBootstrap_Defaults(t *testing.T) {
	clearBootstrapEnv(t)

	boot, err := LoadBootstrap("")
	require.NoError(t, err)
	assert.Equal(t, "http://localhost:15000", boot.Hugr.URL)
	assert.Equal(t, "http://localhost:15000/mcp", boot.Hugr.MCPUrl)
	assert.Equal(t, 10000, boot.A2A.Port)
	assert.Equal(t, 10001, boot.DevUI.Port)
	assert.Equal(t, "http://localhost:10000", boot.A2A.BaseURL)
	assert.False(t, boot.Remote(), "no token/url → local mode")
}

func TestLoadBootstrap_RemoteDetection(t *testing.T) {
	clearBootstrapEnv(t)
	t.Setenv("HUGR_ACCESS_TOKEN", "seed-token")
	t.Setenv("HUGR_TOKEN_URL", "http://hub/exchange")

	boot, err := LoadBootstrap("")
	require.NoError(t, err)
	assert.True(t, boot.Remote(), "both vars set → remote mode")
	assert.Equal(t, "seed-token", boot.HugrAuth.AccessToken)
	assert.Equal(t, "http://hub/exchange", boot.HugrAuth.TokenURL)
	assert.Equal(t, "hugr", boot.HugrAuth.Name)
	assert.Equal(t, "hugr", boot.HugrAuth.Type)
}

func TestLoadBootstrap_AsymmetricEnvRejected(t *testing.T) {
	t.Run("token without url", func(t *testing.T) {
		clearBootstrapEnv(t)
		t.Setenv("HUGR_ACCESS_TOKEN", "tok")
		_, err := LoadBootstrap("")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "HUGR_TOKEN_URL")
	})

	t.Run("url without token", func(t *testing.T) {
		clearBootstrapEnv(t)
		t.Setenv("HUGR_TOKEN_URL", "http://hub/x")
		_, err := LoadBootstrap("")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "HUGR_ACCESS_TOKEN")
	})
}

func TestLoadBootstrap_ReadsEnvFile(t *testing.T) {
	clearBootstrapEnv(t)
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	require.NoError(t, os.WriteFile(envPath, []byte(
		"HUGR_URL=http://example:18000\n"+
			"AGENT_PORT=20000\n"+
			"AGENT_DEVUI_PORT=20001\n",
	), 0o644))

	boot, err := LoadBootstrap(envPath)
	require.NoError(t, err)
	assert.Equal(t, "http://example:18000", boot.Hugr.URL)
	assert.Equal(t, "http://example:18000/mcp", boot.Hugr.MCPUrl)
	assert.Equal(t, 20000, boot.A2A.Port)
	assert.Equal(t, 20001, boot.DevUI.Port)
	assert.Equal(t, "http://localhost:20000", boot.A2A.BaseURL)
	assert.False(t, boot.Remote())
}

func TestLoadBootstrap_HugrMCPURLDerived(t *testing.T) {
	clearBootstrapEnv(t)
	t.Setenv("HUGR_URL", "http://hugr:9000")
	_, err := LoadBootstrap("")
	require.NoError(t, err)
	// Side effect: HUGR_MCP_URL in process env is filled when empty so
	// skills referencing ${HUGR_MCP_URL} resolve.
	assert.Equal(t, "http://hugr:9000/mcp", os.Getenv("HUGR_MCP_URL"))
}

func TestLoadBootstrap_HugrMCPURLRespected(t *testing.T) {
	clearBootstrapEnv(t)
	t.Setenv("HUGR_URL", "http://hugr:9000")
	t.Setenv("HUGR_MCP_URL", "http://mcp-override:7000")
	_, err := LoadBootstrap("")
	require.NoError(t, err)
	assert.Equal(t, "http://mcp-override:7000", os.Getenv("HUGR_MCP_URL"))
}

func TestRemote_NilReceiver(t *testing.T) {
	var b *BootstrapConfig
	assert.False(t, b.Remote(), "nil boot must not panic")
}

// clearBootstrapEnv wipes every env var LoadBootstrap reads so tests
// start from a predictable baseline. t.Setenv restores prior values
// at teardown.
func clearBootstrapEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"HUGR_URL",
		"HUGR_MCP_URL",
		"HUGR_ACCESS_TOKEN",
		"HUGR_TOKEN_URL",
		"AGENT_PORT",
		"AGENT_DEVUI_PORT",
		"AGENT_BASE_URL",
		"AGENT_DEVUI_BASE_URL",
	} {
		t.Setenv(k, "")
	}
}
