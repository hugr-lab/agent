// Package sessions implements the runtime SessionManager and Session,
// plus the ADK session.Service so a single type owns both hugr-specific
// session state (active skills, tools, refs) and ADK-visible
// conversation state/events.
package sessions

import (
	"iter"
	"maps"
	"strings"
	"sync"

	"github.com/hugr-lab/hugen/pkg/skills"
	adksession "google.golang.org/adk/session"
)

// Well-known state keys exposed through the ADK State interface. Values
// for these keys are routed to the typed fields on State; any other key
// goes into the generic kv map.
const (
	KeySkills       = "session.skills"
	KeyTools        = "session.tools"
	KeyRefs         = "session.refs"
	KeyMaxTokens    = "session.max_tokens"
	KeyUsedTokens   = "session.used_tokens"
	KeyCatalog      = "session.catalog" // []skills.SkillMeta
	KeyCatalogIsSet = "session.catalog_set"
)

// State is the per-session state blob. Typed fields cover everything the
// agent inspects directly; `kv` holds anything else ADK or plugins write
// through Set. Any change bumps `dirty` so the next Snapshot rebuilds.
//
// State implements adksession.State.
type State struct {
	mu sync.RWMutex

	// Domain.
	Skills []string
	Tools  []string
	Refs   []string // "skill/ref" pairs

	// SkillVersions tracks the version string last loaded per skill.
	// Populated by LoadSkill; drives version-drift detection — when a
	// skill is re-loaded with a different version, the old tool
	// bindings are dropped before the new ones get built.
	// Not persisted: on process restart every skill's first load
	// re-computes the binding set from scratch.
	SkillVersions map[string]string

	// Catalog override: when CatalogSet is true, CatalogSkills is shown
	// in the prompt; otherwise the manager's default catalog is used.
	CatalogSkills []skills.SkillMeta
	CatalogSet    bool

	// Token budget (calibrateTokens callback updates these).
	MaxContextTokens int
	ContextUsed      int

	// kv is the opaque overflow for arbitrary ADK state keys.
	kv map[string]any

	// dirty flips true on any mutation; cleared by the session after a
	// successful Snapshot rebuild.
	dirty bool
}

var _ adksession.State = (*State)(nil)

// NewState returns an empty state with dirty=true so the first Snapshot
// is always computed.
func NewState() *State {
	return &State{
		kv:            make(map[string]any),
		SkillVersions: make(map[string]string),
		dirty:         true,
	}
}

// LoadedSkillVersion returns the last loaded version string for the
// named skill and whether the skill was loaded at all.
func (s *State) LoadedSkillVersion(name string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.SkillVersions == nil {
		return "", false
	}
	v, ok := s.SkillVersions[name]
	return v, ok
}

// RecordSkillVersion stores the just-loaded version for name.
func (s *State) RecordSkillVersion(name, version string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.SkillVersions == nil {
		s.SkillVersions = make(map[string]string)
	}
	s.SkillVersions[name] = version
	s.dirty = true
}

// ForgetSkillVersion removes the tracked version for name.
func (s *State) ForgetSkillVersion(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.SkillVersions == nil {
		return
	}
	if _, ok := s.SkillVersions[name]; ok {
		delete(s.SkillVersions, name)
		s.dirty = true
	}
}

// Dirty reports whether any typed field was mutated since the last
// MarkClean. Callers hold no locks — State serializes internally.
func (s *State) Dirty() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dirty
}

// MarkClean clears the dirty flag (called after the Snapshot rebuild).
func (s *State) MarkClean() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dirty = false
}

// MarkDirty forces the next Snapshot to rebuild (used when dependencies
// outside State change, e.g. skills.Manager BindTools).
func (s *State) MarkDirty() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dirty = true
}

// ------------------------------------------------------------
// adksession.State
// ------------------------------------------------------------

// Get returns the value for a well-known typed field, falling through to
// the kv map. Mirrors ADK's contract: ErrStateKeyNotExist on miss.
func (s *State) Get(key string) (any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	switch key {
	case KeySkills:
		return cloneStrings(s.Skills), nil
	case KeyTools:
		return cloneStrings(s.Tools), nil
	case KeyRefs:
		return cloneStrings(s.Refs), nil
	case KeyMaxTokens:
		return s.MaxContextTokens, nil
	case KeyUsedTokens:
		return s.ContextUsed, nil
	case KeyCatalog:
		return append([]skills.SkillMeta(nil), s.CatalogSkills...), nil
	case KeyCatalogIsSet:
		return s.CatalogSet, nil
	}
	v, ok := s.kv[key]
	if !ok {
		return nil, adksession.ErrStateKeyNotExist
	}
	return v, nil
}

// Set routes well-known keys to typed fields (with best-effort type
// coercion) and stashes everything else in kv. Always flips dirty.
func (s *State) Set(key string, value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dirty = true
	switch key {
	case KeySkills:
		s.Skills = toStringSlice(value)
	case KeyTools:
		s.Tools = toStringSlice(value)
	case KeyRefs:
		s.Refs = toStringSlice(value)
	case KeyMaxTokens:
		s.MaxContextTokens = toInt(value)
	case KeyUsedTokens:
		s.ContextUsed = toInt(value)
	case KeyCatalog:
		if v, ok := value.([]skills.SkillMeta); ok {
			s.CatalogSkills = append([]skills.SkillMeta(nil), v...)
		}
	case KeyCatalogIsSet:
		if v, ok := value.(bool); ok {
			s.CatalogSet = v
		}
	default:
		s.kv[key] = value
	}
	return nil
}

// All yields every key-value pair — typed fields first, then kv entries.
func (s *State) All() iter.Seq2[string, any] {
	s.mu.RLock()
	skillNames := cloneStrings(s.Skills)
	toolNames := cloneStrings(s.Tools)
	refs := cloneStrings(s.Refs)
	maxT := s.MaxContextTokens
	used := s.ContextUsed
	catalog := append([]skills.SkillMeta(nil), s.CatalogSkills...)
	catSet := s.CatalogSet
	kv := maps.Clone(s.kv)
	s.mu.RUnlock()

	return func(yield func(string, any) bool) {
		if !yield(KeySkills, skillNames) {
			return
		}
		if !yield(KeyTools, toolNames) {
			return
		}
		if !yield(KeyRefs, refs) {
			return
		}
		if !yield(KeyMaxTokens, maxT) {
			return
		}
		if !yield(KeyUsedTokens, used) {
			return
		}
		if !yield(KeyCatalog, catalog) {
			return
		}
		if !yield(KeyCatalogIsSet, catSet) {
			return
		}
		for k, v := range kv {
			if !yield(k, v) {
				return
			}
		}
	}
}

// ------------------------------------------------------------
// Typed mutators (used by Session methods).
// ------------------------------------------------------------

// AddSkill appends name to Skills if not already present. Returns true if
// it was actually added.
func (s *State) AddSkill(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.Skills {
		if n == name {
			return false
		}
	}
	s.Skills = append(s.Skills, name)
	s.dirty = true
	return true
}

// RemoveSkill drops name from Skills. Returns true if something was
// removed.
func (s *State) RemoveSkill(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, n := range s.Skills {
		if n == name {
			s.Skills = append(s.Skills[:i], s.Skills[i+1:]...)
			s.dirty = true
			return true
		}
	}
	return false
}

// AddTools appends any names not already in Tools.
func (s *State) AddTools(names []string) {
	if len(names) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing := make(map[string]struct{}, len(s.Tools))
	for _, n := range s.Tools {
		existing[n] = struct{}{}
	}
	for _, n := range names {
		if _, ok := existing[n]; ok {
			continue
		}
		s.Tools = append(s.Tools, n)
		existing[n] = struct{}{}
		s.dirty = true
	}
}

// RemoveTools drops each name from Tools.
func (s *State) RemoveTools(names []string) {
	if len(names) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	drop := make(map[string]struct{}, len(names))
	for _, n := range names {
		drop[n] = struct{}{}
	}
	out := s.Tools[:0]
	for _, n := range s.Tools {
		if _, ok := drop[n]; ok {
			s.dirty = true
			continue
		}
		out = append(out, n)
	}
	s.Tools = out
}

// AddRef appends "skill/ref" if not already present.
func (s *State) AddRef(skill, ref string) bool {
	key := skill + "/" + ref
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.Refs {
		if r == key {
			return false
		}
	}
	s.Refs = append(s.Refs, key)
	s.dirty = true
	return true
}

// RemoveRef drops "skill/ref" from state. Returns true if something was
// removed.
func (s *State) RemoveRef(skill, ref string) bool {
	key := skill + "/" + ref
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, r := range s.Refs {
		if r == key {
			s.Refs = append(s.Refs[:i], s.Refs[i+1:]...)
			s.dirty = true
			return true
		}
	}
	return false
}

// RemoveRefsForSkill drops every ref belonging to the given skill. Used
// when a skill is unloaded.
func (s *State) RemoveRefsForSkill(skill string) int {
	prefix := skill + "/"
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.Refs[:0]
	removed := 0
	for _, r := range s.Refs {
		if strings.HasPrefix(r, prefix) {
			removed++
			s.dirty = true
			continue
		}
		out = append(out, r)
	}
	s.Refs = out
	return removed
}

// SetCatalog replaces the catalog and marks the session as override.
func (s *State) SetCatalog(list []skills.SkillMeta) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.CatalogSkills = append([]skills.SkillMeta(nil), list...)
	s.CatalogSet = true
	s.dirty = true
}

// ClearCatalog wipes the override and returns to the manager's default.
func (s *State) ClearCatalog() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.CatalogSkills = nil
	s.CatalogSet = true // stay set — "explicitly empty"
	s.dirty = true
}

// SetTokenUsage updates the context-usage counters. No dirty flip — prompt
// doesn't depend on these.
func (s *State) SetTokenUsage(used int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ContextUsed = used
}

// ------------------------------------------------------------
// helpers
// ------------------------------------------------------------

func cloneStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func toStringSlice(v any) []string {
	switch val := v.(type) {
	case []string:
		return cloneStrings(val)
	case []any:
		out := make([]string, 0, len(val))
		for _, x := range val {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func toInt(v any) int {
	switch val := v.(type) {
	case int:
		return val
	case int32:
		return int(val)
	case int64:
		return int(val)
	case float64:
		return int(val)
	}
	return 0
}
