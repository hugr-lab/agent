//go:build duckdb_arrow && scenario

package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	adksession "google.golang.org/adk/session"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/runner"
	"google.golang.org/genai"
)

// TurnResult captures what the agent did during a single user→agent
// round trip. It's a coarse per-turn summary — the full transcript
// lives in hub.db and is surfaced via Inspector.Dump.
type TurnResult struct {
	FinalText  string
	ToolCalls  []ToolCallSummary
	NumEvents  int
	NumTurns   int // terminal model events (TurnComplete=true)
	Duration   time.Duration
	TimedOut   bool
	Err        error
}

// ToolCallSummary condenses one FunctionCall into log-friendly text.
type ToolCallSummary struct {
	Name string
	Args string
}

// CreateSession opens a new coordinator (root) session via the
// manager. Safe to call once per scenario — subsequent RunTurn's
// on the same session id will continue the existing transcript.
func (a *Agent) CreateSession(ctx context.Context, sessionID string) error {
	if a == nil || a.Runtime == nil || a.Runtime.Sessions == nil {
		return fmt.Errorf("harness: nil agent")
	}
	_, err := a.Runtime.Sessions.Create(ctx, &adksession.CreateRequest{
		AppName:   a.AppName,
		UserID:    a.UserID,
		SessionID: sessionID,
	})
	return err
}

// RunTurn fires one user message and waits for the agent's turn to
// complete (or the 60s cap to hit). Logs per-step progress into
// t.Log so the scenario output reads top-to-bottom like a transcript.
func (a *Agent) RunTurn(ctx context.Context, t *testing.T, sessionID, userMsg string) TurnResult {
	t.Helper()
	require.NotNil(t, a, "harness: nil agent")
	require.NotNil(t, a.Runtime.Agent, "harness: nil ADK agent")
	require.NotEmpty(t, sessionID)

	r, err := runner.New(runner.Config{
		AppName:        a.AppName,
		Agent:          a.Runtime.Agent,
		SessionService: a.Runtime.Sessions,
	})
	require.NoError(t, err, "harness: build runner")

	msg := &genai.Content{
		Role:  "user",
		Parts: []*genai.Part{{Text: userMsg}},
	}

	const turnBudget = 60 * time.Second
	turnCtx, cancel := context.WithTimeout(ctx, turnBudget)
	defer cancel()

	start := time.Now()
	tr := TurnResult{}

	t.Logf("── USER ──▶ %s: %s", sessionID, singleLine(userMsg, 240))

	for ev, runErr := range r.Run(turnCtx, a.UserID, sessionID, msg, agent.RunConfig{}) {
		if runErr != nil {
			tr.Err = runErr
			t.Logf("── ERR ──  %v", runErr)
			break
		}
		if ev == nil {
			continue
		}
		tr.NumEvents++
		logEvent(t, ev, &tr)
		if _, done := agentTurnText(ev); done {
			tr.NumTurns++
			// One turn per RunTurn — bail out the moment the model
			// finishes speaking so we're not stuck on the stream tail.
			if !hasFunctionCall(ev) {
				break
			}
		}
	}
	tr.Duration = time.Since(start)
	if turnCtx.Err() == context.DeadlineExceeded {
		tr.TimedOut = true
		t.Errorf("harness: turn did not complete within %s", turnBudget)
	}
	t.Logf("── DONE ── events=%d turns=%d duration=%s timed_out=%v",
		tr.NumEvents, tr.NumTurns, tr.Duration.Truncate(time.Millisecond), tr.TimedOut)
	return tr
}

// logEvent writes a one-line summary per ADK event.
func logEvent(t *testing.T, ev *adksession.Event, tr *TurnResult) {
	if ev.Content == nil {
		return
	}
	role := ev.Content.Role
	for _, p := range ev.Content.Parts {
		if p == nil {
			continue
		}
		switch {
		case p.FunctionCall != nil:
			args := "{}"
			if p.FunctionCall.Args != nil {
				if b, err := json.Marshal(p.FunctionCall.Args); err == nil {
					args = string(b)
				}
			}
			tr.ToolCalls = append(tr.ToolCalls, ToolCallSummary{Name: p.FunctionCall.Name, Args: args})
			t.Logf("   → tool_call   %s %s", p.FunctionCall.Name, singleLine(args, 240))
		case p.FunctionResponse != nil:
			resp := "(empty)"
			if p.FunctionResponse.Response != nil {
				if b, err := json.Marshal(p.FunctionResponse.Response); err == nil {
					resp = string(b)
				}
			}
			t.Logf("   ← tool_result %s %s", p.FunctionResponse.Name, singleLine(resp, 240))
		case p.Text != "":
			if role == "model" || role == "agent" {
				if ev.TurnComplete {
					tr.FinalText = p.Text
				}
				t.Logf("   ▸ %s: %s", role, singleLine(p.Text, 240))
			}
		}
	}
}

// agentTurnText / hasFunctionCall mirror pkg/agent/subagent.go helpers —
// duplicated here to keep the harness decoupled from pkg/agent
// internals (which aren't exported).

func agentTurnText(ev *adksession.Event) (string, bool) {
	if ev == nil || ev.Content == nil {
		return "", false
	}
	if ev.Content.Role != "model" && ev.Content.Role != "agent" {
		return "", false
	}
	if !ev.TurnComplete {
		return "", false
	}
	var b strings.Builder
	for _, p := range ev.Content.Parts {
		if p == nil || p.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(p.Text)
	}
	return b.String(), true
}

func hasFunctionCall(ev *adksession.Event) bool {
	if ev == nil || ev.Content == nil {
		return false
	}
	for _, p := range ev.Content.Parts {
		if p != nil && p.FunctionCall != nil {
			return true
		}
	}
	return false
}

func singleLine(s string, maxRunes int) string {
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…"
}
