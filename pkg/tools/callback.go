package tools

import (
	"fmt"

	"github.com/hugr-lab/hugen/interfaces"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

// Inject returns a BeforeModelCallback that, on every LLM turn, reads the
// current session's Snapshot and rewrites req.Config.Tools + req.Tools.
//
// The agent is built with Toolsets: nil — this callback is the single
// source of truth for both the LLM-visible function declarations and the
// dispatch map. It bypasses ADK's Flow-level `f.Tools` cache
// (see google.golang.org/adk/internal/llminternal/tools_processor.go:31),
// which would otherwise pin the tool list to whatever was loaded on the
// first turn and hide skills loaded mid-invocation.
//
// Session isolation is preserved — the callback resolves Session via the
// SessionManager using ctx.SessionID().
func Inject(sm interfaces.SessionManager) llmagent.BeforeModelCallback {
	return func(ctx agent.CallbackContext, req *model.LLMRequest) (*model.LLMResponse, error) {
		sid := ctx.SessionID()
		if sid == "" {
			return nil, fmt.Errorf("tools: no session id in callback context")
		}
		sess, err := sm.Session(sid)
		if err != nil {
			return nil, fmt.Errorf("tools: session %s: %w", sid, err)
		}
		snap := sess.Snapshot()

		req.Tools = make(map[string]any, len(snap.Tools))
		if req.Config == nil {
			req.Config = &genai.GenerateContentConfig{}
		}
		req.Config.Tools = nil
		for _, t := range snap.Tools {
			Pack(req, t)
		}
		return nil, nil
	}
}
