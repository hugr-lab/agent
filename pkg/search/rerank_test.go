package search

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestApplyRecencyRerank — given two equally-similar hits at
// different ages, the newer one wins under default half-life. With
// an absurdly long half-life, the slightly-better-distance hit
// pulls ahead even when older.
func TestApplyRecencyRerank(t *testing.T) {
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	d1 := 0.20
	d2 := 0.20
	d3 := 0.15

	t.Run("equal distance: newer wins", func(t *testing.T) {
		hits := []SearchHit{
			{ID: "old", CreatedAt: now.Add(-2 * time.Hour), Distance: &d1},
			{ID: "new", CreatedAt: now.Add(-2 * time.Minute), Distance: &d2},
		}
		out := applyRecencyRerank(hits, time.Hour, now, 0)
		assert.Equal(t, "new", out[0].ID)
		assert.Equal(t, "old", out[1].ID)
	})

	t.Run("long half-life: better-distance wins despite age", func(t *testing.T) {
		hits := []SearchHit{
			{ID: "old_better", CreatedAt: now.Add(-2 * time.Hour), Distance: &d3},
			{ID: "new_worse", CreatedAt: now.Add(-2 * time.Minute), Distance: &d1},
		}
		out := applyRecencyRerank(hits, 168*time.Hour, now, 0)
		assert.Equal(t, "old_better", out[0].ID)
	})

	t.Run("nil distance treated as relevance=1", func(t *testing.T) {
		hits := []SearchHit{
			{ID: "no_distance", CreatedAt: now.Add(-1 * time.Minute), Distance: nil},
			{ID: "with_distance", CreatedAt: now.Add(-1 * time.Minute), Distance: &d1},
		}
		out := applyRecencyRerank(hits, time.Hour, now, 0)
		// Same age, but "no_distance" gets relevance=1.0 vs 0.80
		assert.Equal(t, "no_distance", out[0].ID)
	})

	t.Run("limit truncates", func(t *testing.T) {
		hits := []SearchHit{
			{ID: "1", CreatedAt: now, Distance: &d1},
			{ID: "2", CreatedAt: now.Add(-1 * time.Hour), Distance: &d1},
			{ID: "3", CreatedAt: now.Add(-2 * time.Hour), Distance: &d1},
		}
		out := applyRecencyRerank(hits, time.Hour, now, 2)
		assert.Len(t, out, 2)
	})
}

// TestApplyRecencyOnly — pure recency ordering, no distance signal.
func TestApplyRecencyOnly(t *testing.T) {
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	hits := []SearchHit{
		{ID: "old", CreatedAt: now.Add(-3 * time.Hour)},
		{ID: "newest", CreatedAt: now.Add(-1 * time.Minute)},
		{ID: "mid", CreatedAt: now.Add(-1 * time.Hour)},
	}
	out := applyRecencyOnly(hits, time.Hour, now, 0)
	assert.Equal(t, []string{"newest", "mid", "old"}, []string{out[0].ID, out[1].ID, out[2].ID})

	// All RecencyBoost values populated, all in (0, 1].
	for _, h := range out {
		assert.Greater(t, h.RecencyBoost, 0.0)
		assert.LessOrEqual(t, h.RecencyBoost, 1.0)
	}
}

// TestScope_Validate exercises the recognised scope set + the
// IsCoordinatorOnly classifier.
func TestScope_Validate(t *testing.T) {
	cases := []struct {
		scope     Scope
		valid     bool
		coordOnly bool
	}{
		{ScopeTurn, true, false},
		{ScopeMission, true, false},
		{ScopeSession, true, true},
		{ScopeUser, true, true},
		{Scope("invalid"), false, false},
		{Scope(""), false, false},
	}
	for _, c := range cases {
		t.Run(string(c.scope), func(t *testing.T) {
			err := c.scope.Validate()
			if c.valid {
				assert.NoError(t, err)
				assert.Equal(t, c.coordOnly, c.scope.IsCoordinatorOnly())
			} else {
				assert.Error(t, err)
			}
		})
	}
}
