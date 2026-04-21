package main

import (
	"context"
	"iter"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	a2acore "github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2aclient"
	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/hugr-lab/hugen/pkg/a2a"
	hugen "github.com/hugr-lab/hugen/pkg/agent"
	"github.com/hugr-lab/hugen/pkg/models"
	"github.com/hugr-lab/hugen/pkg/sessions"
	"github.com/hugr-lab/hugen/pkg/skills"
	"github.com/hugr-lab/hugen/pkg/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/artifact"
	"google.golang.org/adk/model"
	adksession "google.golang.org/adk/session"
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
	sessionSvc := adksession.InMemoryService()
	artifactSvc := artifact.InMemoryService()

	cardH, invokeH := a2a.BuildHandlers(a, sessionSvc, artifactSvc, baseURL)
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

	result, err := client.SendMessage(context.Background(), &a2acore.MessageSendParams{
		Message: &a2acore.Message{
			ID:    "msg-1",
			Role:  a2acore.MessageRoleUser,
			Parts: []a2acore.Part{a2acore.TextPart{Text: "hi"}},
		},
	})
	require.NoError(t, err)
	task := result.(*a2acore.Task)

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
	cardH, invokeH := a2a.BuildHandlers(a, adksession.InMemoryService(), artifact.InMemoryService(), baseURL)
	mux := http.NewServeMux()
	mux.Handle(a2asrv.WellKnownAgentCardPath, cardH)
	mux.Handle("/invoke", invokeH)
	go http.Serve(listener, mux)

	resp, err := http.Get(baseURL + a2asrv.WellKnownAgentCardPath)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/json")
}

// writeTestSkills creates an on-disk skills directory for the file manager.
func writeTestSkills(t *testing.T, specs map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, md := range specs {
		sd := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(sd, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(sd, "SKILL.md"), []byte(md), 0o644))
	}
	return dir
}

// testHugrAgentConfig configures startTestHugrAgent.
type testHugrAgentConfig struct {
	llm          *models.ScriptedLLM
	constitution string
	skillsDir    string // optional; if set, used for skills.NewFileManager
	tokens       *models.TokenEstimator
}

// startTestHugrAgent builds the full HugrAgent stack (skills + tools +
// session + agent) for tests and returns an a2aclient.Client.
func startTestHugrAgent(t *testing.T, llm *models.ScriptedLLM, constitution string) *a2aclient.Client {
	t.Helper()
	client, _ := startTestHugrAgentWithConfig(t, testHugrAgentConfig{
		llm:          llm,
		constitution: constitution,
	})
	return client
}

func startTestHugrAgentWithConfig(t *testing.T, cfg testHugrAgentConfig) (*a2aclient.Client, *models.TokenEstimator) {
	t.Helper()
	logger := slog.Default()

	skillsDir := cfg.skillsDir
	if skillsDir == "" {
		skillsDir = t.TempDir()
	}
	skillsMgr, err := skills.NewFileManager(skillsDir)
	require.NoError(t, err)

	toolsMgr := tools.New(logger)

	sessionMgr := sessions.New(sessions.Config{
		Skills:       skillsMgr,
		Tools:        toolsMgr,
		Constitution: cfg.constitution,
		Logger:       logger,
	})

	// Register the real skills.Service so the test exercises the same
	// provider wiring as production.
	toolsMgr.AddProvider(skills.NewService(skillsSessionAdapter{sm: sessionMgr}))

	tokens := cfg.tokens
	if tokens == nil {
		tokens = models.NewTokenEstimator()
	}

	router := models.NewRouter(cfg.llm)
	a, err := hugen.NewAgent(hugen.Runtime{
		Router:   router,
		Sessions: sessionMgr,
		Tokens:   tokens,
		Logger:   logger,
	})
	require.NoError(t, err)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	baseURL := "http://" + listener.Addr().String()
	artifactSvc := artifact.InMemoryService()

	cardH, invokeH := a2a.BuildHandlers(a, sessionMgr, artifactSvc, baseURL)
	mux := http.NewServeMux()
	mux.Handle(a2asrv.WellKnownAgentCardPath, cardH)
	mux.Handle("/invoke", invokeH)

	go http.Serve(listener, mux)
	t.Cleanup(func() { listener.Close() })

	client, err := a2aclient.NewFromEndpoints(context.Background(), []a2acore.AgentInterface{
		{URL: baseURL + "/invoke", Transport: a2acore.TransportProtocolJSONRPC},
	})
	require.NoError(t, err)
	return client, tokens
}

func TestHugrAgent_SendMessage(t *testing.T) {
	llm := models.NewScriptedLLM("test", []models.ScriptedResponse{
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
	require.True(t, ok)
	assert.Equal(t, a2acore.TaskStateCompleted, task.Status.State)

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
	llm := models.NewScriptedLLM("test", []models.ScriptedResponse{
		{Content: "Calibrated response"},
	})
	client, tokens := startTestHugrAgentWithConfig(t, testHugrAgentConfig{
		llm:          llm,
		constitution: "constitution text",
	})

	_, err := client.SendMessage(context.Background(), &a2acore.MessageSendParams{
		Message: &a2acore.Message{
			ID:    "msg-1",
			Role:  a2acore.MessageRoleUser,
			Parts: []a2acore.Part{a2acore.TextPart{Text: "calibrate"}},
		},
	})
	require.NoError(t, err)

	assert.Equal(t, "measured", tokens.Source())
	pt, ct := tokens.LastUsage()
	assert.Equal(t, 100, pt)
	assert.Equal(t, 50, ct)
}

func TestHugrAgent_SkillLifecycle(t *testing.T) {
	// Scripted LLM: turn 1 → skill_list, turn 2 → skill_load, turn 3 → final.
	llm := models.NewScriptedLLM("test", []models.ScriptedResponse{
		{ToolCalls: []models.ScriptedToolCall{{Name: "skill_list", Args: map[string]any{}}}},
		{ToolCalls: []models.ScriptedToolCall{{Name: "skill_load", Args: map[string]any{"name": "test-data"}}}},
		{Content: "I loaded the test-data skill and I'm ready to help."},
	})

	skillsDir := writeTestSkills(t, map[string]string{
		"test-data": `---
name: test-data
description: Test data skill
---

# Test Data

You can query test data.
`,
	})

	client, _ := startTestHugrAgentWithConfig(t, testHugrAgentConfig{
		llm:          llm,
		constitution: "You are a test agent.",
		skillsDir:    skillsDir,
	})

	result, err := client.SendMessage(context.Background(), &a2acore.MessageSendParams{
		Message: &a2acore.Message{
			ID:    "msg-1",
			Role:  a2acore.MessageRoleUser,
			Parts: []a2acore.Part{a2acore.TextPart{Text: "show me the data"}},
		},
	})
	require.NoError(t, err)

	task, ok := result.(*a2acore.Task)
	require.True(t, ok)
	assert.Equal(t, a2acore.TaskStateCompleted, task.Status.State)

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
