package artifacts

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestVisibility_OrderAndWidening(t *testing.T) {
	cases := []struct {
		v        Visibility
		order    int
		canWiden Visibility
		expect   bool
	}{
		{VisibilitySelf, 0, VisibilityParent, true},
		{VisibilitySelf, 0, VisibilityGraph, true},
		{VisibilitySelf, 0, VisibilityUser, true},
		{VisibilityParent, 1, VisibilityGraph, true},
		{VisibilityGraph, 2, VisibilityUser, true},
		{VisibilityUser, 3, VisibilityGraph, false}, // narrowing
		{VisibilityParent, 1, VisibilitySelf, false},
		{VisibilityGraph, 2, VisibilityGraph, false}, // no-op
		{Visibility("bad"), -1, VisibilitySelf, false},
	}
	for _, c := range cases {
		t.Run(string(c.v)+"→"+string(c.canWiden), func(t *testing.T) {
			assert.Equal(t, c.order, c.v.Order())
			assert.Equal(t, c.expect, c.v.CanWidenTo(c.canWiden))
		})
	}

	assert.True(t, VisibilitySelf.IsValid())
	assert.True(t, VisibilityUser.IsValid())
	assert.False(t, Visibility("nope").IsValid())
}

func TestTTL_IsValid(t *testing.T) {
	for _, v := range []TTL{TTLSession, TTL7d, TTL30d, TTLPermanent} {
		assert.True(t, v.IsValid(), "%s should be valid", v)
	}
	assert.False(t, TTL("forever").IsValid())
	assert.False(t, TTL("").IsValid())
}

func TestPublishSource_HasPathHasInline(t *testing.T) {
	s := PublishSource{}
	assert.False(t, s.HasPath())
	assert.False(t, s.HasInline())

	s = PublishSource{Path: "/x"}
	assert.True(t, s.HasPath())
	assert.False(t, s.HasInline())

	s = PublishSource{InlineBytes: []byte("y")}
	assert.False(t, s.HasPath())
	assert.True(t, s.HasInline())
}
