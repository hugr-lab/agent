package memory

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseReviewOutput_Strict(t *testing.T) {
	raw := `{"facts": [{"content": "tf.incidents has 14 fields", "category": "schema", "volatility": "stable", "tags": ["tf","schema"]}], "hypotheses": []}`
	got, err := parseReviewOutput(raw)
	require.NoError(t, err)
	require.Len(t, got.Facts, 1)
	assert.Equal(t, "tf.incidents has 14 fields", got.Facts[0].Content)
	assert.Equal(t, "schema", got.Facts[0].Category)
	assert.ElementsMatch(t, []string{"tf", "schema"}, got.Facts[0].Tags)
}

func TestParseReviewOutput_CodeFence(t *testing.T) {
	raw := "```json\n" + `{"facts": [], "hypotheses": [{"content": "severity has 3 values", "priority": "high", "verification": "query distinct", "estimated_calls": 2}]}` + "\n```"
	got, err := parseReviewOutput(raw)
	require.NoError(t, err)
	require.Len(t, got.Hypotheses, 1)
	assert.Equal(t, "severity has 3 values", got.Hypotheses[0].Content)
}

func TestParseReviewOutput_NoJSON(t *testing.T) {
	_, err := parseReviewOutput("sorry, I can't help")
	require.Error(t, err)
}

func TestEqualEnough(t *testing.T) {
	assert.True(t, equalEnough("abc", "abc"))
	assert.True(t, equalEnough("ABC", "abc"))
	assert.True(t, equalEnough("the quick brown fox jumps over", "the quick brown fox jumps ov"))
	assert.False(t, equalEnough("abc", "xyz"))
	assert.False(t, equalEnough("", "abc"))
}

func TestDurationFor_Defaults(t *testing.T) {
	r := &Reviewer{}
	assert.Equal(t, int64(24), int64(r.durationFor("volatile").Hours()))
	assert.Equal(t, int64(168), int64(r.durationFor("fast").Hours()))
	assert.Equal(t, int64(720), int64(r.durationFor("moderate").Hours()))
	assert.Equal(t, int64(2160), int64(r.durationFor("slow").Hours()))
	assert.Equal(t, int64(8760), int64(r.durationFor("stable").Hours()))
	assert.Equal(t, int64(8760), int64(r.durationFor("unknown").Hours()))
}
