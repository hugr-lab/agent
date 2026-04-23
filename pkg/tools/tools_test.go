package tools

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/adk/tool"
)

// cacheable wraps FakeProvider with a counting Invalidate to prove
// the Manager calls it through the CacheableProvider interface.
type cacheable struct {
	FakeProvider
	invalidated int
}

func (c *cacheable) Invalidate() { c.invalidated++ }

func TestManager_AddProviderAndLookup(t *testing.T) {
	m := New(nil)
	p := FakeProvider{N: "p1", T: FakeTools("one", "two")}
	m.AddProvider(p)

	got, err := m.Provider("p1")
	require.NoError(t, err)
	assert.Equal(t, "p1", got.Name())

	// Nil provider is ignored.
	m.AddProvider(nil)

	// Unknown name errors.
	_, err = m.Provider("nope")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not registered")
}

func TestManager_AddProvider_ReplacesByName(t *testing.T) {
	m := New(nil)
	m.AddProvider(FakeProvider{N: "p", T: FakeTools("t1")})
	m.AddProvider(FakeProvider{N: "p", T: FakeTools("t2")})
	tools, err := m.ProviderTools("p")
	require.NoError(t, err)
	names := make([]string, 0, len(tools))
	for _, tt := range tools {
		names = append(names, tt.Name())
	}
	assert.Equal(t, []string{"t2"}, names, "second AddProvider must replace")
}

func TestManager_RemoveProvider(t *testing.T) {
	m := New(nil)
	m.AddProvider(FakeProvider{N: "p1", T: FakeTools("a")})

	require.NoError(t, m.RemoveProvider("p1"))
	_, err := m.Provider("p1")
	require.Error(t, err, "provider must be gone after RemoveProvider")

	// Second Remove is a no-op.
	require.NoError(t, m.RemoveProvider("p1"))

	// Empty name rejected.
	err = m.RemoveProvider("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires a name")
}

func TestManager_ProviderTools(t *testing.T) {
	m := New(nil)
	m.AddProvider(FakeProvider{N: "p", T: FakeTools("alpha", "beta")})

	tools, err := m.ProviderTools("p")
	require.NoError(t, err)
	assert.Len(t, tools, 2)

	_, err = m.ProviderTools("missing")
	require.Error(t, err)
}

func TestManager_Tools_AllVsFiltered(t *testing.T) {
	m := New(nil)
	m.AddProvider(FakeProvider{N: "a", T: FakeTools("x", "y")})
	m.AddProvider(FakeProvider{N: "b", T: FakeTools("y", "z")})

	all, err := m.Tools()
	require.NoError(t, err)
	names := collectNames(all)
	// Tools() across providers does NOT dedup — returns the full flat
	// list; dedup happens only in the filtered path. This keeps the
	// LLM-facing catalogue honest about duplicate-name tools.
	assert.ElementsMatch(t, []string{"x", "y", "y", "z"}, names)

	// Filtered: dedup by name, first occurrence wins.
	filtered, err := m.Tools("y", "z")
	require.NoError(t, err)
	names = collectNames(filtered)
	assert.ElementsMatch(t, []string{"y", "z"}, names)

	// Unknown names silently dropped.
	filtered, err = m.Tools("ghost", "x")
	require.NoError(t, err)
	names = collectNames(filtered)
	assert.Equal(t, []string{"x"}, names)
}

func TestManager_AllNames_Dedup(t *testing.T) {
	m := New(nil)
	m.AddProvider(FakeProvider{N: "a", T: FakeTools("x", "y")})
	m.AddProvider(FakeProvider{N: "b", T: FakeTools("y", "z")})

	names := m.AllNames()
	sort.Strings(names)
	assert.Equal(t, []string{"x", "y", "z"}, names)
}

func TestManager_ProviderNames(t *testing.T) {
	m := New(nil)
	m.AddProvider(FakeProvider{N: "a"})
	m.AddProvider(FakeProvider{N: "b"})
	m.AddProvider(FakeProvider{N: "c"})

	names := m.ProviderNames()
	sort.Strings(names)
	assert.Equal(t, []string{"a", "b", "c"}, names)
}

func TestManager_InvalidateProvider_Cacheable(t *testing.T) {
	m := New(nil)
	c := &cacheable{FakeProvider: FakeProvider{N: "c", T: FakeTools("t")}}
	m.AddProvider(c)

	require.NoError(t, m.InvalidateProvider("c"))
	assert.Equal(t, 1, c.invalidated)

	// Unknown name errors.
	err := m.InvalidateProvider("nope")
	require.Error(t, err)
}

func TestManager_InvalidateProvider_PlainIsNoOp(t *testing.T) {
	m := New(nil)
	m.AddProvider(FakeProvider{N: "plain", T: FakeTools("t")})
	// Non-cacheable provider → no error, no panic.
	require.NoError(t, m.InvalidateProvider("plain"))
}

func TestManager_InvalidateAll(t *testing.T) {
	m := New(nil)
	c1 := &cacheable{FakeProvider: FakeProvider{N: "c1"}}
	c2 := &cacheable{FakeProvider: FakeProvider{N: "c2"}}
	m.AddProvider(c1)
	m.AddProvider(c2)
	m.AddProvider(FakeProvider{N: "plain"})

	m.InvalidateAll()
	assert.Equal(t, 1, c1.invalidated)
	assert.Equal(t, 1, c2.invalidated)
}

// collectNames extracts tool names in order for assertions.
func collectNames(ts []tool.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name())
	}
	return out
}
