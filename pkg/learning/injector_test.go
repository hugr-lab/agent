package learning

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestInjectorCache_TTL(t *testing.T) {
	c := &injectorCache{entries: map[string]injectorEntry{}}

	c.put("s1", "hello")
	assert.Equal(t, "hello", c.get("s1"))

	// Force expiry.
	c.entries["s1"] = injectorEntry{expires: time.Now().Add(-1 * time.Second), text: "stale"}
	assert.Empty(t, c.get("s1"))

	// Missing sid.
	assert.Empty(t, c.get("missing"))
}

func TestInjectorCache_PutOverwrites(t *testing.T) {
	c := &injectorCache{entries: map[string]injectorEntry{}}
	c.put("s1", "v1")
	c.put("s1", "v2")
	assert.Equal(t, "v2", c.get("s1"))
}
