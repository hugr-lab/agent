package tools

import (
	"strings"

	"google.golang.org/adk/tool"
)

// FilteredProvider is a stateless decorator that exposes a subset of
// another Provider's tools. It implements Provider and forwards
// Invalidate to the underlying raw provider when that one is a
// CacheableProvider — so multiple filter views over the same raw share
// a single cache.
//
// Allow patterns support exact names and a single trailing `*`
// (prefix glob). Examples:
//
//	["discovery-search_data_sources"]   // exact
//	["discovery-*", "schema-type_fields"] // prefix + exact mixed
//
// An empty or nil allowlist means "pass through every tool from raw".
type FilteredProvider struct {
	name  string
	raw   Provider
	allow []string
}

var (
	_ Provider          = (*FilteredProvider)(nil)
	_ CacheableProvider = (*FilteredProvider)(nil)
)

// NewFiltered wraps raw under a new provider name with an optional
// allowlist. The allowlist is applied on every Tools() call against
// raw.Tools(), so it tracks upstream updates without any local state.
func NewFiltered(name string, raw Provider, allow []string) *FilteredProvider {
	return &FilteredProvider{name: name, raw: raw, allow: append([]string(nil), allow...)}
}

// Name returns the filter-view name (typically "skill/<skill>/<provider>").
func (p *FilteredProvider) Name() string { return p.name }

// RawName returns the underlying provider's name — useful for admin
// tooling that wants to show "view X over raw Y".
func (p *FilteredProvider) RawName() string { return p.raw.Name() }

// Tools returns raw.Tools() filtered by the allowlist.
func (p *FilteredProvider) Tools() []tool.Tool {
	tools := p.raw.Tools()
	if len(p.allow) == 0 {
		return tools
	}
	out := make([]tool.Tool, 0, len(tools))
	for _, t := range tools {
		if matchesAny(t.Name(), p.allow) {
			out = append(out, t)
		}
	}
	return out
}

// Invalidate forwards to raw when it caches; otherwise no-op.
func (p *FilteredProvider) Invalidate() {
	if cp, ok := p.raw.(CacheableProvider); ok {
		cp.Invalidate()
	}
}

// matchesAny returns true if name matches any pattern. A pattern
// ending in "*" is a prefix glob (matches "prefix-anything"); every
// other pattern must match exactly.
func matchesAny(name string, patterns []string) bool {
	for _, p := range patterns {
		if strings.HasSuffix(p, "*") {
			if strings.HasPrefix(name, strings.TrimSuffix(p, "*")) {
				return true
			}
		} else if p == name {
			return true
		}
	}
	return false
}
