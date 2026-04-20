package learning

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseVerdict_Strict(t *testing.T) {
	raw := `{"verdict":"confirmed","evidence":"all 3 distinct values seen","volatility":"stable"}`
	got, err := parseVerdict(raw)
	require.NoError(t, err)
	assert.Equal(t, "confirmed", got.Verdict)
	assert.Equal(t, "all 3 distinct values seen", got.Evidence)
	assert.Equal(t, "stable", got.Volatility)
}

func TestParseVerdict_CodeFence(t *testing.T) {
	raw := "```json\n{\"verdict\":\"rejected\",\"evidence\":\"actually 5 values\"}\n```"
	got, err := parseVerdict(raw)
	require.NoError(t, err)
	assert.Equal(t, "rejected", got.Verdict)
}

func TestParseVerdict_NoJSON(t *testing.T) {
	_, err := parseVerdict("I can't verify this right now")
	assert.Error(t, err)
}

func TestVerifierDurationFor_Defaults(t *testing.T) {
	v := &Verifier{}
	assert.Equal(t, int64(24), int64(v.durationFor("volatile").Hours()))
	assert.Equal(t, int64(8760), int64(v.durationFor("stable").Hours()))
	assert.Equal(t, int64(8760), int64(v.durationFor("").Hours()))
}
