package providers

import (
	"errors"
	"testing"

	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/tools"
	"github.com/hugr-lab/hugen/pkg/tools/toolstest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildAll_DispatchAndRegister(t *testing.T) {
	// Register a local type so the test doesn't depend on mcp/system
	// network/SessionManager wiring. Overwriting is allowed.
	Register("test-static", func(cfg config.ProviderConfig, _ Deps) (tools.Provider, error) {
		return toolstest.Provider{N: cfg.Name, T: toolstest.Tools(cfg.Name + "_t")}, nil
	})

	tm := tools.New(nil)
	err := BuildAll([]config.ProviderConfig{
		{Name: "alpha", Type: "test-static"},
		{Name: "beta", Type: "test-static"},
	}, tm, Deps{})
	require.NoError(t, err)

	names := tm.ProviderNames()
	assert.ElementsMatch(t, []string{"alpha", "beta"}, names)

	got, err := tm.ProviderTools("alpha")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "alpha_t", got[0].Name())
}

func TestBuildAll_UnknownType(t *testing.T) {
	tm := tools.New(nil)
	err := BuildAll([]config.ProviderConfig{
		{Name: "x", Type: "nope"},
	}, tm, Deps{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown type")
}

func TestBuildAll_NameMismatch(t *testing.T) {
	Register("test-wrongname", func(cfg config.ProviderConfig, _ Deps) (tools.Provider, error) {
		return toolstest.Provider{N: "different", T: nil}, nil
	})
	tm := tools.New(nil)
	err := BuildAll([]config.ProviderConfig{
		{Name: "expected", Type: "test-wrongname"},
	}, tm, Deps{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mismatch")
}

func TestBuildAll_BuilderError(t *testing.T) {
	Register("test-fail", func(cfg config.ProviderConfig, _ Deps) (tools.Provider, error) {
		return nil, errors.New("builder exploded")
	})
	tm := tools.New(nil)
	err := BuildAll([]config.ProviderConfig{
		{Name: "boom", Type: "test-fail"},
	}, tm, Deps{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "builder exploded")
}

func TestRegisteredTypes_ContainsBuiltins(t *testing.T) {
	types := RegisteredTypes()
	assert.Contains(t, types, "mcp")
	assert.Contains(t, types, "system")
}
