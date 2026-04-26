package approvals

import (
	"encoding/json"
	"fmt"
	"strings"
)

// renderEnvelopeBody produces the user-facing Markdown body of an
// approval_requested event. The body is the same one the LLM
// surfaces to the user verbatim per the coordinator constitution.
//
// Format mirrors contracts/envelope.md.
func renderEnvelopeBody(meta EnvelopeMetadata, args map[string]any) string {
	var b strings.Builder

	switch meta.HITLKind {
	case HITLKindApproval:
		b.WriteString("🔒 **Approval needed**\n\n")
		fmt.Fprintf(&b, "**Mission:** mission `%s`\n", meta.MissionID)
		fmt.Fprintf(&b, "**Tool:** `%s`\n", meta.ToolName)
		fmt.Fprintf(&b, "**Risk:** %s\n", meta.Risk)
		if len(meta.EstimatedImpact) > 0 {
			fmt.Fprintf(&b, "**Expected impact:** %s\n", formatImpact(meta.EstimatedImpact))
		}
		b.WriteString("\n**Arguments:**\n```")
		b.WriteString(argsLang(meta.ToolName))
		b.WriteString("\n")
		b.WriteString(argsRendered(meta.ToolName, args))
		b.WriteString("\n```\n\n")
		b.WriteString("Reply with:\n")
		fmt.Fprintf(&b, "- `approve %s` — run the tool as-is\n", meta.ApprovalID)
		fmt.Fprintf(&b, "- `reject %s because <reason>` — cancel the mission\n", meta.ApprovalID)
		fmt.Fprintf(&b, "- `modify %s <new args as JSON>` — re-run with user-supplied args\n", meta.ApprovalID)
	case HITLKindAsk:
		b.WriteString("❓ **Question from sub-agent**\n\n")
		fmt.Fprintf(&b, "_(mission `%s`)_\n\n", meta.MissionID)
		question := stringFromArgs(args, "question")
		if question != "" {
			b.WriteString(question)
			b.WriteString("\n")
		}
		if len(meta.Suggested) > 0 {
			b.WriteString("\n**Suggested:**\n")
			for _, s := range meta.Suggested {
				fmt.Fprintf(&b, "- `%s`\n", s)
			}
		}
		b.WriteString("\nReply with:\n")
		fmt.Fprintf(&b, "- `answer %s <your-answer>` — free-form text, or one of the suggestions above\n", meta.ApprovalID)
	}
	return b.String()
}

// argsLang selects a fenced-block language for the args block based
// on the tool name. Matches contracts/envelope.md.
func argsLang(toolName string) string {
	switch {
	case strings.HasPrefix(toolName, "data-"):
		return "graphql"
	case strings.HasPrefix(toolName, "python-"):
		return "python"
	case strings.HasPrefix(toolName, "web-"):
		return "bash"
	default:
		return "json"
	}
}

// argsRendered formats the args object for the Markdown body. For
// data-* tools we render a representative GraphQL fragment when the
// args contain a `statement` key; for others we pretty-print JSON.
func argsRendered(toolName string, args map[string]any) string {
	if strings.HasPrefix(toolName, "data-") {
		if stmt, ok := args["statement"].(string); ok {
			return fmt.Sprintf("mutation {\n  data { execute_mutation(\n    statement: %q\n  ) { affected_rows } }\n}", stmt)
		}
	}
	pretty, err := json.MarshalIndent(args, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", args)
	}
	return string(pretty)
}

// formatImpact renders an impact map (e.g. {affected_rows: 278}) as
// a short human-readable string. Multiple keys are joined with ", ".
func formatImpact(m map[string]any) string {
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	return strings.Join(parts, ", ")
}

// argsDigest returns a short string preview of the args map suitable
// for the EnvelopeMetadata.ArgsDigest field. Truncated to ~200 chars
// so the events stream stays lean (full args live on the approvals
// row, not in event content).
func argsDigest(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	// Heuristic: prefer a `statement` or `query` field when present;
	// fall back to a JSON-encoded preview.
	for _, key := range []string{"statement", "query", "question"} {
		if v, ok := args[key].(string); ok && v != "" {
			return truncateRunes(v, 200)
		}
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return truncateRunes(string(encoded), 200)
}

// truncateRunes truncates s to at most n runes, appending "…" when
// truncation actually happened.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ToMap converts EnvelopeMetadata to a map[string]any suitable for
// the session_events.metadata column. Empty fields are dropped.
func (m EnvelopeMetadata) ToMap() map[string]any {
	out := map[string]any{
		"hitl_kind":   string(m.HITLKind),
		"approval_id": m.ApprovalID,
		"mission_id":  m.MissionID,
		"tool_name":   m.ToolName,
		"risk":        string(m.Risk),
		"choices":     m.Choices,
	}
	if m.ArgsDigest != "" {
		out["args_digest"] = m.ArgsDigest
	}
	if len(m.EstimatedImpact) > 0 {
		out["estimated_impact"] = m.EstimatedImpact
	}
	if len(m.Suggested) > 0 {
		out["suggested"] = m.Suggested
	}
	return out
}
