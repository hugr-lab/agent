//go:build duckdb_arrow

package store_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentstore "github.com/hugr-lab/hugen/pkg/agent/store"
	"github.com/hugr-lab/hugen/pkg/store/testenv"
)

// TestLoadConfigFromHub_SeededAgent uses the testenv defaults: the
// seeded agent (agt_ag01) has no config_override, so the merged
// result is just agent_type.config with identity fields filled from
// the agents row.
func TestLoadConfigFromHub_SeededAgent(t *testing.T) {
	service, _ := testenv.Engine(t)
	merged, row, err := agentstore.LoadConfigFromHub(context.Background(), service, "agt_ag01")
	require.NoError(t, err)
	require.NotNil(t, row)

	// Seed config is {"constitution":"test"} from testenv.Engine.
	assert.Equal(t, "test", merged["constitution"])

	// Identity fields are populated from the agents row.
	assert.Equal(t, "agt_ag01", row.ID)
	assert.Equal(t, "hugr-data", row.AgentTypeID)
	assert.Equal(t, "ag01", row.ShortID)
	assert.Equal(t, "hugr-data-agent-test", row.Name)
}

// TestLoadConfigFromHub_OverrideWins registers an agent with a
// config_override that stomps on a subset of agent_type.config
// keys; the merge should prefer the override for shared keys and
// preserve the defaults elsewhere.
func TestLoadConfigFromHub_OverrideWins(t *testing.T) {
	service, _ := testenv.Engine(t)
	logger := slog.New(slog.NewTextHandler(discardWriter{}, nil))

	reg, err := agentstore.New(service, agentstore.Options{
		AgentID: "agt_ag99", AgentShort: "ag99", Logger: logger,
	})
	require.NoError(t, err)
	require.NoError(t, reg.RegisterAgent(context.Background(), agentstore.Agent{
		ID: "agt_ag99", AgentTypeID: "hugr-data", ShortID: "ag99",
		Name: "override-agent",
		ConfigOverride: map[string]any{
			"constitution": "override-const",
			"llm":          map[string]any{"model": "custom"},
		},
	}))

	merged, row, err := agentstore.LoadConfigFromHub(context.Background(), service, "agt_ag99")
	require.NoError(t, err)
	require.NotNil(t, row)

	// constitution overridden; llm added on top of agent_type.config.
	assert.Equal(t, "override-const", merged["constitution"])
	llm, ok := merged["llm"].(map[string]any)
	require.True(t, ok, "llm key should survive decode as map")
	assert.Equal(t, "custom", llm["model"])
}

func TestLoadConfigFromHub_MissingAgentReturnsError(t *testing.T) {
	service, _ := testenv.Engine(t)
	_, _, err := agentstore.LoadConfigFromHub(context.Background(), service, "agt_ghost")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not registered in hub")
}

func TestLoadConfigFromHub_InputValidation(t *testing.T) {
	service, _ := testenv.Engine(t)

	_, _, err := agentstore.LoadConfigFromHub(context.Background(), nil, "agt_ag01")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil querier")

	_, _, err = agentstore.LoadConfigFromHub(context.Background(), service, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty agent ID")
}
