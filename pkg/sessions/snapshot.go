package sessions

import "google.golang.org/adk/tool"

// Snapshot is the per-turn view of a session: assembled system prompt
// plus the flat list of tools the LLM should see. Returned by
// Session.Snapshot() and consumed by Inject (BeforeModelCallback) and
// the agent's InstructionProvider.
type Snapshot struct {
	Prompt string
	Tools  []tool.Tool
}
