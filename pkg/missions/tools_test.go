package missions

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/hugr-lab/hugen/pkg/missions/graph"
)

func TestSubRunsLimit_Defaults(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int
	}{
		{"nil", nil, 20},
		{"zero", float64(0), 20},
		{"negative", float64(-3), 20},
		{"valid", float64(15), 15},
		{"int valid", 7, 7},
		{"over max", float64(80), 50},
		{"int over max", 999, 50},
		{"unsupported type", "fifteen", 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, subRunsLimit(tc.in))
		})
	}
}

func TestDigestString(t *testing.T) {
	assert.Equal(t, "short", digestString("short", 200))
	assert.Equal(t, "abc…", digestString("abcdef", 3))
	assert.Equal(t, "", digestString("", 200))
	// n<=0 returns the input unchanged.
	assert.Equal(t, "untouched", digestString("untouched", 0))
}

func TestDigestArgs(t *testing.T) {
	got := digestArgs(map[string]any{"k": "v"}, 200)
	assert.Equal(t, `{"k":"v"}`, got)

	// Truncation kicks in past the limit.
	long := strings.Repeat("a", 50)
	gotLong := digestArgs(map[string]any{"k": long}, 16)
	assert.True(t, strings.HasSuffix(gotLong, "…"), "truncated args should end with ellipsis")

	assert.Equal(t, "", digestArgs(nil, 200))
	assert.Equal(t, "", digestArgs(map[string]any{}, 200))
}

func TestRenderTree_OrdersByDeps(t *testing.T) {
	nodes := []graph.MissionRecord{
		{ID: "b", Role: "downstream", Task: "depends on a", Status: graph.StatusPending,
			DependsOn: []string{"a"}},
		{ID: "a", Role: "upstream", Task: "no deps", Status: graph.StatusRunning, TurnsUsed: 2},
	}
	out := renderTree(nodes)
	lines := strings.Split(out, "\n")
	assert.Len(t, lines, 2)
	assert.Contains(t, lines[0], "upstream")
	assert.Contains(t, lines[0], "RUNNING")
	assert.Contains(t, lines[0], "(turn 2)")
	assert.Contains(t, lines[1], "downstream")
	assert.Contains(t, lines[1], "PENDING")
}

func TestRenderTree_Empty(t *testing.T) {
	assert.Equal(t, "No missions running.", renderTree(nil))
}

func TestErrorEnvelope(t *testing.T) {
	got := errorEnvelope("bad")
	assert.Equal(t, map[string]any{"error": "bad"}, got)
}
