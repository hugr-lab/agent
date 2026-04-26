package approvals

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestArgsLang(t *testing.T) {
	cases := []struct {
		toolName string
		want     string
	}{
		{"data-execute_mutation", "graphql"},
		{"data-list_modules", "graphql"},
		{"python-execute", "python"},
		{"web-fetch", "bash"},
		{"random_tool", "json"},
		{"", "json"},
	}
	for _, c := range cases {
		t.Run(c.toolName, func(t *testing.T) {
			assert.Equal(t, c.want, argsLang(c.toolName))
		})
	}
}

func TestArgsDigest(t *testing.T) {
	t.Run("statement preferred", func(t *testing.T) {
		got := argsDigest(map[string]any{
			"statement": "DELETE FROM x WHERE y=1",
			"limit":     100,
		})
		assert.Equal(t, "DELETE FROM x WHERE y=1", got)
	})
	t.Run("query preferred when statement missing", func(t *testing.T) {
		got := argsDigest(map[string]any{
			"query": "SELECT 1",
			"limit": 100,
		})
		assert.Equal(t, "SELECT 1", got)
	})
	t.Run("question preferred for ask", func(t *testing.T) {
		got := argsDigest(map[string]any{
			"question": "Which table?",
		})
		assert.Equal(t, "Which table?", got)
	})
	t.Run("falls back to JSON", func(t *testing.T) {
		got := argsDigest(map[string]any{
			"foo": "bar",
		})
		assert.Equal(t, `{"foo":"bar"}`, got)
	})
	t.Run("truncates long strings", func(t *testing.T) {
		long := strings.Repeat("x", 250)
		got := argsDigest(map[string]any{"statement": long})
		// truncateRunes appends … on truncation
		assert.True(t, len(got) <= 250+3, "got length %d", len(got))
		assert.True(t, strings.HasSuffix(got, "…"))
	})
	t.Run("empty args returns empty string", func(t *testing.T) {
		assert.Equal(t, "", argsDigest(nil))
		assert.Equal(t, "", argsDigest(map[string]any{}))
	})
}

func TestRenderEnvelopeBody_Approval(t *testing.T) {
	meta := EnvelopeMetadata{
		HITLKind:   HITLKindApproval,
		ApprovalID: "app-7c9d",
		MissionID:  "sess_mis_42",
		ToolName:   "data-execute_mutation",
		Risk:       RiskHigh,
		Choices:    []string{"approve", "reject", "modify"},
		EstimatedImpact: map[string]any{
			"affected_rows": 278,
		},
	}
	args := map[string]any{
		"statement": "DELETE FROM incidents WHERE date < '2025-01-01'",
	}
	body := renderEnvelopeBody(meta, args)

	assert.Contains(t, body, "🔒 **Approval needed**")
	assert.Contains(t, body, "mission `sess_mis_42`")
	assert.Contains(t, body, "`data-execute_mutation`")
	assert.Contains(t, body, "**Risk:** high")
	assert.Contains(t, body, "affected_rows=278")
	assert.Contains(t, body, "DELETE FROM incidents")
	assert.Contains(t, body, "approve app-7c9d")
	assert.Contains(t, body, "reject app-7c9d because <reason>")
	assert.Contains(t, body, "modify app-7c9d <new args as JSON>")
	assert.Contains(t, body, "```graphql")
}

func TestRenderEnvelopeBody_Ask(t *testing.T) {
	meta := EnvelopeMetadata{
		HITLKind:   HITLKindAsk,
		ApprovalID: "app-8a0c",
		MissionID:  "sess_mis_9",
		Choices:    []string{"answer"},
		Suggested:  []string{"tf.incidents", "safety.incidents"},
	}
	args := map[string]any{
		"question": "Which table?",
	}
	body := renderEnvelopeBody(meta, args)

	assert.Contains(t, body, "❓ **Question from sub-agent**")
	assert.Contains(t, body, "Which table?")
	assert.Contains(t, body, "`tf.incidents`")
	assert.Contains(t, body, "answer app-8a0c")
}

func TestEnvelopeMetadata_ToMap(t *testing.T) {
	meta := EnvelopeMetadata{
		HITLKind:   HITLKindApproval,
		ApprovalID: "app-1",
		MissionID:  "m-1",
		ToolName:   "x",
		Risk:       RiskLow,
		Choices:    []string{"approve"},
		ArgsDigest: "preview",
	}
	m := meta.ToMap()
	assert.Equal(t, "approval", m["hitl_kind"])
	assert.Equal(t, "app-1", m["approval_id"])
	assert.Equal(t, "low", m["risk"])
	assert.Equal(t, "preview", m["args_digest"])
	// Empty fields excluded.
	_, hasImpact := m["estimated_impact"]
	assert.False(t, hasImpact)
}

func TestNewApprovalID(t *testing.T) {
	a := NewApprovalID()
	b := NewApprovalID()
	assert.NotEqual(t, a, b)
	assert.True(t, strings.HasPrefix(a, "app-"))
	assert.True(t, strings.HasPrefix(b, "app-"))
	// 12 hex chars after "app-" → total length 4 + 12 = 16.
	assert.Equal(t, 16, len(a))
}
