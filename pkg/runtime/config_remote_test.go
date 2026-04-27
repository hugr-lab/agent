package runtime

import (
	"context"
	"testing"

	"github.com/hugr-lab/query-engine/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hugr-lab/hugen/pkg/identity/hub"
)

// stubQuerier returns a pre-populated types.Response for every Query
// call. Routing is coarse: the Query call returns queued responses in
// FIFO order so multiple calls per test (agents → agent_types) each
// get their own payload.
type stubQuerier struct {
	responses []*types.Response
	err       error
	calls     int
}

func (s *stubQuerier) Query(_ context.Context, _ string, _ map[string]any) (*types.Response, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.calls >= len(s.responses) {
		return &types.Response{Data: map[string]any{}}, nil
	}
	r := s.responses[s.calls]
	s.calls++
	return r, nil
}

// Unused Querier methods — LoadRemote only calls Query.
func (s *stubQuerier) Subscribe(context.Context, string, map[string]any) (*types.Subscription, error) {
	return nil, nil
}
func (s *stubQuerier) RegisterDataSource(context.Context, types.DataSource) error    { return nil }
func (s *stubQuerier) LoadDataSource(context.Context, string) error                  { return nil }
func (s *stubQuerier) UnloadDataSource(context.Context, string, ...types.UnloadOpt) error {
	return nil
}
func (s *stubQuerier) DataSourceStatus(context.Context, string) (string, error)         { return "", nil }
func (s *stubQuerier) DescribeDataSource(context.Context, string, bool) (string, error) { return "", nil }

// agentsResponse returns what hub.db.agents(id=$id) looks like for a
// single hit. The nested hub.db shape is what queries.RunQuery scans.
func agentsResponse(row map[string]any) *types.Response {
	return &types.Response{Data: map[string]any{
		"hub": map[string]any{
			"db": map[string]any{
				"agents": []any{row},
			},
		},
	}}
}

func agentTypesResponse(row map[string]any) *types.Response {
	return &types.Response{Data: map[string]any{
		"hub": map[string]any{
			"db": map[string]any{
				"agent_types": []any{row},
			},
		},
	}}
}

func baseBoot(t *testing.T) *BootstrapConfig {
	clearBootstrapEnv(t)
	boot, err := LoadBootstrap("")
	require.NoError(t, err)
	return boot
}

func TestLoadRemote_MergesAgentTypeAndOverride(t *testing.T) {
	boot := baseBoot(t)

	// agents row: overrides llm.model, leaves skills alone.
	agentRow := map[string]any{
		"id":            "agt_1",
		"agent_type_id": "hugr-data",
		"short_id":      "ag01",
		"name":          "inst-1",
		"status":        "active",
		"config_override": map[string]any{
			"llm": map[string]any{"model": "override-model"},
		},
	}
	agentTypeRow := map[string]any{
		"config": map[string]any{
			"llm":    map[string]any{"model": "default-model", "max_tokens": 4096},
			"skills": map[string]any{"path": "./skills"},
		},
	}

	q := &stubQuerier{responses: []*types.Response{
		agentsResponse(agentRow),
		agentTypesResponse(agentTypeRow),
	}}

	cfg, err := LoadRemote(context.Background(), hub.NewWithAgent(q, "agt_1"), boot)
	require.NoError(t, err)

	// Override wins for llm.model; top-level skills from agent_type
	// survives because config_override didn't touch it.
	assert.Equal(t, "override-model", cfg.LLM.Model)
	assert.Equal(t, "./skills", cfg.Skills.Path)

	// Identity fills in from agents row (bootstrap had empty identity).
	assert.Equal(t, "agt_1", cfg.Identity.ID)
	assert.Equal(t, "ag01", cfg.Identity.ShortID)
	assert.Equal(t, "inst-1", cfg.Identity.Name)
	assert.Equal(t, "hugr-data", cfg.Identity.Type)

	// Hugr block passed through from bootstrap, not remote config.
	assert.Equal(t, boot.Hugr.URL, cfg.Hugr.URL)
}

func TestLoadRemote_MissingAgentErrors(t *testing.T) {
	boot := baseBoot(t)

	// agents query returns an empty list → LoadConfigFromHub errors.
	q := &stubQuerier{responses: []*types.Response{
		{Data: map[string]any{
			"hub": map[string]any{"db": map[string]any{"agents": []any{}}},
		}},
	}}

	_, err := LoadRemote(context.Background(), hub.NewWithAgent(q, "ghost"), boot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not registered in hub")
}

func TestLoadRemote_AuthEnvExpansion(t *testing.T) {
	boot := baseBoot(t)
	t.Setenv("MY_WEATHER_KEY", "sekret-42")

	agentRow := map[string]any{
		"id": "agt_2", "agent_type_id": "hugr-data", "short_id": "ag02",
		"name": "inst-2", "status": "active",
		"config_override": map[string]any{},
	}
	agentTypeRow := map[string]any{
		"config": map[string]any{
			"auth": []any{
				map[string]any{
					"name":              "weather",
					"type":              "oidc",
					"issuer":            "http://idp",
					"client_id":         "wx",
					"access_token":      "${MY_WEATHER_KEY}",
				},
			},
		},
	}

	q := &stubQuerier{responses: []*types.Response{
		agentsResponse(agentRow),
		agentTypesResponse(agentTypeRow),
	}}

	cfg, err := LoadRemote(context.Background(), hub.NewWithAgent(q, "agt_2"), boot)
	require.NoError(t, err)
	require.Len(t, cfg.Auth, 1)
	// ${MY_WEATHER_KEY} expanded to the process-env value.
	assert.Equal(t, "sekret-42", cfg.Auth[0].AccessToken)
}

func TestLoadRemote_InputValidation(t *testing.T) {
	_, err := LoadRemote(context.Background(), nil, &BootstrapConfig{})
	require.ErrorContains(t, err, "identity.Source")

	q := &stubQuerier{}
	_, err = LoadRemote(context.Background(), hub.NewWithAgent(q, "x"), nil)
	require.ErrorContains(t, err, "BootstrapConfig")
}
