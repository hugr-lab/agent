package artifacts

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSortRefsByDistanceAsc(t *testing.T) {
	d := func(v float64) *float64 { return &v }
	refs := []ArtifactRef{
		{ID: "c", DistanceToQuery: d(0.30)},
		{ID: "a", DistanceToQuery: d(0.10)},
		{ID: "nil-tail", DistanceToQuery: nil},
		{ID: "b", DistanceToQuery: d(0.20)},
	}
	sortRefsByDistanceAsc(refs)
	got := []string{refs[0].ID, refs[1].ID, refs[2].ID, refs[3].ID}
	assert.Equal(t, []string{"a", "b", "c", "nil-tail"}, got,
		"smallest distance first; nil distance sorts last")
}

func TestDistanceLess(t *testing.T) {
	d := func(v float64) *float64 { return &v }
	cases := []struct {
		a, b *float64
		want bool
	}{
		{d(0.1), d(0.2), true},
		{d(0.2), d(0.1), false},
		{d(0.1), d(0.1), false},
		{nil, d(0.5), false},
		{d(0.5), nil, true},
		{nil, nil, false},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, distanceLess(c.a, c.b),
			"a=%v b=%v", c.a, c.b)
	}
}

func TestTagsContainAll(t *testing.T) {
	cases := []struct {
		have, want []string
		ok         bool
	}{
		{[]string{"a", "b"}, nil, true},
		{[]string{"a", "b"}, []string{"a"}, true},
		{[]string{"a", "b"}, []string{"a", "b"}, true},
		{[]string{"a"}, []string{"a", "b"}, false},
		{nil, []string{"a"}, false},
		{nil, nil, true},
	}
	for _, c := range cases {
		assert.Equal(t, c.ok, tagsContainAll(c.have, c.want),
			"have=%v want=%v", c.have, c.want)
	}
}

func TestScopeRank(t *testing.T) {
	// Order matters for ListVisible dedup.
	assert.Less(t, scopeRank("self"), scopeRank("grant"))
	assert.Less(t, scopeRank("grant"), scopeRank("parent"))
	assert.Less(t, scopeRank("parent"), scopeRank("graph"))
	assert.Less(t, scopeRank("graph"), scopeRank("user"))
	assert.Equal(t, -1, scopeRank("nope"))
}
