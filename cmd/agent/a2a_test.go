package main

import (
	"context"
	"iter"
	"log/slog"
	"net"
	"net/http"
	"testing"

	a2acore "github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2aclient"
	"github.com/a2aproject/a2a-go/a2asrv"
	testadapters "github.com/hugr-lab/hugen/adapters/test"
	"github.com/hugr-lab/hugen/interfaces"
	hugen "github.com/hugr-lab/hugen/pkg/agent"
	"github.com/hugr-lab/hugen/pkg/llms/intent"
	"github.com/hugr-lab/hugen/pkg/tools/system"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/artifact"
	"google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// mockLLM returns a fixed text response.
type mockLLM struct{ response string }

func (m *mockLLM) Name() string { return "mock" }
func (m *mockLLM) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content: &genai.Content{
				Role:  "model",
				Parts: []*genai.Part{{Text: m.response}},
			},
			TurnComplete: true,
		}, nil)
	}
}

// startTestA2AServer creates a test A2A server with a mock LLM and returns
// an a2aclient.Client connected to it.
func startTestA2AServer(t *testing.T, llmResponse string) *a2aclient.Client {
	t.Helper()

	a, err := llmagent.New(llmagent.Config{
		Name:        "test_agent",
		Description: "Test agent",
		Model:       &mockLLM{response: llmResponse},
		Instruction: "You are a test agent.",
	})
	require.NoError(t, err)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	baseURL := "http://" + listener.Addr().String()
	sessionSvc := session.InMemoryService()
	artifactSvc := artifact.InMemoryService()

	cardH, invokeH := a2aHandlers(a, sessionSvc, artifactSvc, baseURL)
	mux := http.NewServeMux()
	mux.Handle(a2asrv.WellKnownAgentCardPath, cardH)
	mux.Handle("/invoke", invokeH)

	go http.Serve(listener, mux)
	t.Cleanup(func() { listener.Close() })

	client, err := a2aclient.NewFromEndpoints(context.Background(), []a2acore.AgentInterface{
		{URL: baseURL + "/invoke", Transport: a2acore.TransportProtocolJSONRPC},
	})
	require.NoError(t, err)
	return client
}

func TestA2A_SendMessage(t *testing.T) {
	client := startTestA2AServer(t, "Hello from test agent!")

	result, err := client.SendMessage(context.Background(), &a2acore.MessageSendParams{
		Message: &a2acore.Message{
			ID:   "msg-1",
			Role: a2acore.MessageRoleUser,
			Parts: []a2acore.Part{
				a2acore.TextPart{Text: "hi"},
			},
		},
	})
	require.NoError(t, err)

	task, ok := result.(*a2acore.Task)
	require.True(t, ok, "expected Task result, got %T", result)
	assert.Equal(t, a2acore.TaskStateCompleted, task.Status.State)
	assert.NotEmpty(t, task.ID)
}

func TestA2A_GetTask(t *testing.T) {
	client := startTestA2AServer(t, "response")

	// Create a task first.
	result, err := client.SendMessage(context.Background(), &a2acore.MessageSendParams{
		Message: &a2acore.Message{
			ID:    "msg-1",
			Role:  a2acore.MessageRoleUser,
			Parts: []a2acore.Part{a2acore.TextPart{Text: "hi"}},
		},
	})
	require.NoError(t, err)
	task := result.(*a2acore.Task)

	// Get the same task.
	got, err := client.GetTask(context.Background(), &a2acore.TaskQueryParams{ID: task.ID})
	require.NoError(t, err)
	assert.Equal(t, task.ID, got.ID)
	assert.Equal(t, a2acore.TaskStateCompleted, got.Status.State)
}

func TestA2A_GetTask_NotFound(t *testing.T) {
	client := startTestA2AServer(t, "response")

	_, err := client.GetTask(context.Background(), &a2acore.TaskQueryParams{ID: "nonexistent"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "task not found")
}

func TestA2A_AgentCard(t *testing.T) {
	a, err := llmagent.New(llmagent.Config{
		Name:        "card_test",
		Description: "Card test agent",
		Model:       &mockLLM{response: "x"},
		Instruction: "test",
	})
	require.NoError(t, err)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	baseURL := "http://" + listener.Addr().String()
	cardH, invokeH := a2aHandlers(a, session.InMemoryService(), artifact.InMemoryService(), baseURL)
	mux := http.NewServeMux()
	mux.Handle(a2asrv.WellKnownAgentCardPath, cardH)
	mux.Handle("/invoke", invokeH)
	go http.Serve(listener, mux)

	// Fetch agent card via HTTP.
	resp, err := http.Get(baseURL + a2asrv.WellKnownAgentCardPath)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/json")
}

// testHugrAgentConfig holds configuration for startTestHugrAgent.
type testHugrAgentConfig struct {
	llm          *testadapters.ScriptedLLM
	constitution string
	skills       interfaces.SkillProvider
}

// startTestHugrAgent creates a test A2A server backed by HugrAgent
// with a ScriptedLLM and returns an a2aclient.Client.
func startTestHugrAgent(t *testing.T, llm *testadapters.ScriptedLLM, constitution string) *a2aclient.Client {
	t.Helper()
	return startTestHugrAgentWithConfig(t, testHugrAgentConfig{
		llm:          llm,
		constitution: constitution,
	})
}

func startTestHugrAgentWithConfig(t *testing.T, cfg testHugrAgentConfig) *a2aclient.Client {
	t.Helper()

	router := intent.NewRouter(cfg.llm)
	prompt := hugen.NewPromptBuilder(cfg.constitution)
	toolset := hugen.NewDynamicToolset()
	tokens := hugen.NewTokenEstimator()

	if cfg.skills != nil {
		sysDeps := &system.Deps{
			Skills:    cfg.skills,
			Prompt:    prompt,
			Toolset:   toolset,
			Tokens:    tokens,
			Transport: http.DefaultTransport,
			Logger:    slog.Default(),
		}
		toolset.AddToolset("system", system.NewSystemToolset(sysDeps))
	}

	a, err := hugen.NewAgent(hugen.AgentConfig{
		Router:  router,
		Toolset: toolset,
		Prompt:  prompt,
		Tokens:  tokens,
		Logger:  slog.Default(),
	})
	require.NoError(t, err)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	baseURL := "http://" + listener.Addr().String()
	sessionSvc := session.InMemoryService()
	artifactSvc := artifact.InMemoryService()

	cardH, invokeH := a2aHandlers(a, sessionSvc, artifactSvc, baseURL)
	mux := http.NewServeMux()
	mux.Handle(a2asrv.WellKnownAgentCardPath, cardH)
	mux.Handle("/invoke", invokeH)

	go http.Serve(listener, mux)
	t.Cleanup(func() { listener.Close() })

	client, err := a2aclient.NewFromEndpoints(context.Background(), []a2acore.AgentInterface{
		{URL: baseURL + "/invoke", Transport: a2acore.TransportProtocolJSONRPC},
	})
	require.NoError(t, err)
	return client
}

func TestHugrAgent_SendMessage(t *testing.T) {
	llm := testadapters.NewScriptedLLM("test", []testadapters.ScriptedResponse{
		{Content: "Hello from HugrAgent!"},
	})
	client := startTestHugrAgent(t, llm, "You are a test agent.")

	result, err := client.SendMessage(context.Background(), &a2acore.MessageSendParams{
		Message: &a2acore.Message{
			ID:   "msg-1",
			Role: a2acore.MessageRoleUser,
			Parts: []a2acore.Part{
				a2acore.TextPart{Text: "hi"},
			},
		},
	})
	require.NoError(t, err)

	task, ok := result.(*a2acore.Task)
	require.True(t, ok, "expected Task result, got %T", result)
	assert.Equal(t, a2acore.TaskStateCompleted, task.Status.State)
	assert.NotEmpty(t, task.ID)

	// Verify artifacts contain the response.
	require.NotEmpty(t, task.Artifacts)
	found := false
	for _, art := range task.Artifacts {
		for _, p := range art.Parts {
			if tp, ok := p.(a2acore.TextPart); ok && tp.Text == "Hello from HugrAgent!" {
				found = true
			}
		}
	}
	assert.True(t, found, "expected response text in artifacts")
}

func TestHugrAgent_TokenCalibration(t *testing.T) {
	tokens := hugen.NewTokenEstimator()

	llm := testadapters.NewScriptedLLM("test", []testadapters.ScriptedResponse{
		{Content: "Calibrated response"},
	})
	router := intent.NewRouter(llm)
	prompt := hugen.NewPromptBuilder("constitution text")
	toolset := hugen.NewDynamicToolset()

	a, err := hugen.NewAgent(hugen.AgentConfig{
		Router:  router,
		Toolset: toolset,
		Prompt:  prompt,
		Tokens:  tokens,
		Logger:  slog.Default(),
	})
	require.NoError(t, err)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	baseURL := "http://" + listener.Addr().String()
	cardH, invokeH := a2aHandlers(a, session.InMemoryService(), artifact.InMemoryService(), baseURL)
	mux := http.NewServeMux()
	mux.Handle(a2asrv.WellKnownAgentCardPath, cardH)
	mux.Handle("/invoke", invokeH)
	go http.Serve(listener, mux)

	client, err := a2aclient.NewFromEndpoints(context.Background(), []a2acore.AgentInterface{
		{URL: baseURL + "/invoke", Transport: a2acore.TransportProtocolJSONRPC},
	})
	require.NoError(t, err)

	_, err = client.SendMessage(context.Background(), &a2acore.MessageSendParams{
		Message: &a2acore.Message{
			ID:   "msg-1",
			Role: a2acore.MessageRoleUser,
			Parts: []a2acore.Part{
				a2acore.TextPart{Text: "calibrate"},
			},
		},
	})
	require.NoError(t, err)

	// After the LLM call, token estimator should have been calibrated.
	assert.Equal(t, "measured", tokens.Source())
	pt, ct := tokens.LastUsage()
	assert.Equal(t, 100, pt, "prompt tokens from ScriptedLLM usage metadata")
	assert.Equal(t, 50, ct, "completion tokens from ScriptedLLM usage metadata")
}

func TestHugrAgent_SkillLifecycle(t *testing.T) {
	// Scripted LLM: turn 1 calls skill_list, turn 2 calls skill_load, turn 3 responds.
	llm := testadapters.NewScriptedLLM("test", []testadapters.ScriptedResponse{
		// Turn 1: LLM calls skill_list.
		{ToolCalls: []testadapters.ScriptedToolCall{
			{Name: "skill_list", Args: map[string]any{}},
		}},
		// Turn 2: LLM calls skill_load with the skill name.
		{ToolCalls: []testadapters.ScriptedToolCall{
			{Name: "skill_load", Args: map[string]any{"name": "test-data"}},
		}},
		// Turn 3: LLM responds with final text.
		{Content: "I loaded the test-data skill and I'm ready to help."},
	})

	skills := testadapters.NewStaticSkillProvider(map[string]*interfaces.SkillFull{
		"test-data": {
			Name:         "test-data",
			Instructions: "# Test Data\nYou can query test data.",
			References: []interfaces.SkillRefMeta{
				{Name: "patterns", Description: "Query patterns"},
			},
		},
	})

	client := startTestHugrAgentWithConfig(t, testHugrAgentConfig{
		llm:          llm,
		constitution: "You are a test agent.",
		skills:       skills,
	})

	result, err := client.SendMessage(context.Background(), &a2acore.MessageSendParams{
		Message: &a2acore.Message{
			ID:   "msg-1",
			Role: a2acore.MessageRoleUser,
			Parts: []a2acore.Part{
				a2acore.TextPart{Text: "show me the data"},
			},
		},
	})
	require.NoError(t, err)

	task, ok := result.(*a2acore.Task)
	require.True(t, ok, "expected Task result, got %T", result)
	assert.Equal(t, a2acore.TaskStateCompleted, task.Status.State)

	// Verify the final response is present.
	found := false
	for _, art := range task.Artifacts {
		for _, p := range art.Parts {
			if tp, ok := p.(a2acore.TextPart); ok && tp.Text == "I loaded the test-data skill and I'm ready to help." {
				found = true
			}
		}
	}
	assert.True(t, found, "expected final response text in artifacts")
}
