package chatcontext

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/genai"
)

// TestSplitAtSafeBoundary_NeverSplitsCallResult: the compactor must
// not strand a FunctionResponse in `tail` without its preceding
// FunctionCall in `oldest`. It adjusts the boundary rightward until
// the safe cut is found.
func TestSplitAtSafeBoundary_NeverSplitsCallResult(t *testing.T) {
	c := &Compactor{}
	// User msg, call, result, user msg, call, result, final msg
	contents := []*genai.Content{
		{Role: "user", Parts: []*genai.Part{{Text: "hi"}}},
		{Role: "model", Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: "t1"}}}},
		{Role: "user", Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{Name: "t1"}}}},
		{Role: "user", Parts: []*genai.Part{{Text: "u2"}}},
		{Role: "model", Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: "t2"}}}},
		{Role: "user", Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{Name: "t2"}}}},
		{Role: "model", Parts: []*genai.Part{{Text: "done"}}},
	}

	// idx=2 would split t1_call from t1_result. Safe boundary advances
	// past the orphan FunctionResponse.
	oldest, tail := c.splitAtSafeBoundary(contents, 2)
	require := assert.New(t)
	require.Equal(3, len(oldest), "oldest should include call+result pair intact")
	// tail starts after the pair.
	require.NotEmpty(tail)
	require.NotNil(tail[0])
	require.Nil(tail[0].Parts[0].FunctionResponse, "tail must not start with an orphan FunctionResponse")
}

func TestSplitAtSafeBoundary_IdxAtStart(t *testing.T) {
	c := &Compactor{}
	contents := []*genai.Content{
		{Role: "user", Parts: []*genai.Part{{Text: "a"}}},
		{Role: "model", Parts: []*genai.Part{{Text: "b"}}},
	}
	oldest, tail := c.splitAtSafeBoundary(contents, 0)
	assert.Empty(t, oldest)
	assert.Len(t, tail, 2)
}

func TestSplitAtSafeBoundary_IdxPastEnd(t *testing.T) {
	c := &Compactor{}
	contents := []*genai.Content{
		{Role: "user", Parts: []*genai.Part{{Text: "a"}}},
	}
	oldest, tail := c.splitAtSafeBoundary(contents, 1)
	assert.Len(t, oldest, 1)
	assert.Empty(t, tail)
}

func TestCarriesFunctionResponse(t *testing.T) {
	c := &Compactor{}
	assert.False(t, c.carriesFunctionResponse(nil))
	assert.False(t, c.carriesFunctionResponse(&genai.Content{Parts: []*genai.Part{{Text: "hi"}}}))
	assert.True(t, c.carriesFunctionResponse(&genai.Content{
		Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{Name: "t"}}},
	}))
}

func TestItoa(t *testing.T) {
	assert.Equal(t, "0", itoa(0))
	assert.Equal(t, "42", itoa(42))
	assert.Equal(t, "-5", itoa(-5))
	assert.Equal(t, "1234567890", itoa(1234567890))
}
