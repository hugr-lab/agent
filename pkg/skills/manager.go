package skills

import (
	"context"
)

// Manager is the skill catalogue abstraction. Implementations must treat
// every call as potentially dynamic — no caching is assumed. A file-based
// catalogue can be hot-edited; a future hub-backed catalogue can push new
// skills at any time.
type Manager interface {
	// List returns compact metadata for every skill currently in the
	// catalogue.
	List(ctx context.Context) ([]SkillMeta, error)

	// Load returns the fully-loaded Skill (instructions, refs, mcp endpoint)
	// for the given name. Returns an error if the skill is unknown.
	Load(ctx context.Context, name string) (*Skill, error)

	// Reference returns the raw content of a skill's reference document.
	Reference(ctx context.Context, skillName, refName string) (string, error)

	// RenderCatalog formats a skill slice as a prompt block — caller
	// decides which skills to show.
	RenderCatalog(skills []SkillMeta) string

	// AutoloadNames returns every skill whose frontmatter sets
	// autoload: true. Called by SessionManager on session Create.
	AutoloadNames(ctx context.Context) ([]string, error)

	// AutoloadNamesFor returns the autoload skills applicable to the
	// given session type (spec 006): a skill applies when its
	// frontmatter's `autoload_for` list includes sessionType. When a
	// skill omits autoload_for the parser defaults to ["root"], so the
	// pre-006 behaviour ("autoload skills land on root sessions only")
	// is preserved automatically.
	AutoloadNamesFor(ctx context.Context, sessionType string) ([]string, error)
}

// Cacheable is the optional interface satisfied by implementations that
// memoise catalogue data. File-backed managers read from disk on every
// call and don't implement this; a future hub-backed manager will — to
// handle "skill catalogue changed" pushes + admin refresh endpoints.
type Cacheable interface {
	Manager

	// Invalidate drops every cache entry. The next List/Load/Reference
	// call re-fetches from the origin.
	Invalidate()

	// InvalidateSkill drops cache for one skill (metadata, instructions,
	// references). Safe to call with an unknown name (no-op).
	InvalidateSkill(name string)
}
