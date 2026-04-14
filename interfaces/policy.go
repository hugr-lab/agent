package interfaces

import "context"

// ToolPolicy controls which tools the agent is allowed to use.
type ToolPolicy interface {
	// Filter returns only the tool names the agent is allowed to use.
	Filter(ctx context.Context, toolNames []string) []string

	// CanCall checks if a specific tool call is permitted.
	CanCall(ctx context.Context, toolName string) bool
}

// AllowAllPolicy permits all tools. Used as default when no policy is configured.
type AllowAllPolicy struct{}

func (AllowAllPolicy) Filter(_ context.Context, toolNames []string) []string { return toolNames }
func (AllowAllPolicy) CanCall(_ context.Context, _ string) bool              { return true }
