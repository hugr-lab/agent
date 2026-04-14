package main

import (
	"context"
	"iter"
	"net"
	"net/http"
	"testing"

	a2acore "github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2aclient"
	"github.com/a2aproject/a2a-go/a2asrv"
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
