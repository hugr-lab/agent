package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/hugr-lab/hugen/interfaces"
	"github.com/hugr-lab/hugen/pkg/skills"
	"github.com/hugr-lab/hugen/pkg/tools"
	"github.com/hugr-lab/hugen/pkg/tools/system"
	adksession "google.golang.org/adk/session"
	"google.golang.org/adk/tool"
)

// Session is a runtime conversation. It implements adksession.Session and
// interfaces.Session. Prompt/tools are resolved at Snapshot time against
// the current skills.Manager and tools.Manager — no caching, so hot-edits
// of skills or newly-registered MCP tools are visible on the next turn.
type Session struct {
	id      string
	appName string
	userID  string

	state  *State
	events *eventStore

	manager *Manager
	skills  skills.Manager
	tools   *tools.Manager
	hub     interfaces.HubDB
	logger  *slog.Logger

	constitution string

	mu        sync.RWMutex
	updatedAt time.Time
}

var (
	_ adksession.Session = (*Session)(nil)
	_ interfaces.Session = (*Session)(nil)
)

type sessionConfig struct {
	id           string
	appName      string
	userID       string
	manager      *Manager
	skills       skills.Manager
	tools        *tools.Manager
	hub          interfaces.HubDB
	logger       *slog.Logger
	constitution string
}

func newSession(cfg sessionConfig) *Session {
	return &Session{
		id:           cfg.id,
		appName:      cfg.appName,
		userID:       cfg.userID,
		state:        NewState(),
		events:       newEventStore(),
		manager:      cfg.manager,
		skills:       cfg.skills,
		tools:        cfg.tools,
		hub:          cfg.hub,
		logger:       cfg.logger,
		constitution: cfg.constitution,
		updatedAt:    time.Now(),
	}
}

// ------------------------------------------------------------
// adksession.Session
// ------------------------------------------------------------

func (s *Session) ID() string                { return s.id }
func (s *Session) AppName() string           { return s.appName }
func (s *Session) UserID() string            { return s.userID }
func (s *Session) State() adksession.State   { return s.state }
func (s *Session) Events() adksession.Events { return s.events }
func (s *Session) LastUpdateTime() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.updatedAt
}

// ------------------------------------------------------------
// interfaces.Session
// ------------------------------------------------------------

// SetCatalog replaces the catalog shown in this session's prompt.
func (s *Session) SetCatalog(list []interfaces.SkillMeta) error {
	s.state.SetCatalog(list)
	s.touch()
	return nil
}

// LoadSkill marks the skill active and registers a FilteredProvider
// per declared SkillProviderSpec. Bindings are keyed deterministically
// (`skill/<name>/<idx>`) so repeated LoadSkill calls are idempotent.
// Tool names are always resolved fresh at Snapshot time — not stashed
// in state.
//
// Atomicity: we bind every provider FIRST. If any binding fails we
// abort before touching state or hub, so a partial load doesn't leave
// a stale skill_loaded event on disk pointing at a skill with no tools.
func (s *Session) LoadSkill(ctx context.Context, name string) error {
	sk, err := s.skills.Load(ctx, name)
	if err != nil {
		return err
	}

	// Bind bindings first (phase 1). Roll back any that were created
	// in this call if a later one fails.
	created := make([]int, 0, len(sk.Providers))
	for i, spec := range sk.Providers {
		existed := false
		if _, err := s.tools.Provider(bindingName(sk.Name, i)); err == nil {
			existed = true
		}
		if err := s.bindSkillProvider(sk.Name, i, spec); err != nil {
			// Roll back: drop only the providers we added in this call,
			// not ones already present from a prior successful load.
			for _, j := range created {
				_ = s.tools.RemoveProvider(bindingName(sk.Name, j))
				if sk.Providers[j].Provider == "" {
					_ = s.tools.RemoveProvider(inlineName(sk.Name, j))
				}
			}
			return fmt.Errorf("load skill %q: provider %d: %w", sk.Name, i, err)
		}
		if !existed {
			created = append(created, i)
		}
	}

	// Phase 2: commit to state + hub. AddSkill is idempotent — if the
	// skill was already active (restore + autoload combo), skip the
	// event write to avoid double-logging on every restart.
	if !s.state.AddSkill(sk.Name) {
		s.touch()
		return nil
	}
	s.touch()

	if s.hub != nil {
		providerNames := make([]string, 0, len(sk.Providers))
		for _, spec := range sk.Providers {
			if spec.Provider != "" {
				providerNames = append(providerNames, spec.Provider)
			} else {
				providerNames = append(providerNames, spec.Endpoint)
			}
		}
		meta, _ := json.Marshal(interfaces.SkillLoadedMeta{
			Skill: sk.Name,
			Tools: providerNames,
		})
		ev := interfaces.SessionEvent{
			SessionID: s.id,
			AgentID:   s.hub.AgentID(),
			EventType: interfaces.EventTypeSkillLoaded,
			Author:    s.id,
			Content:   sk.Name,
			Metadata:  jsonToMap(meta),
		}
		if _, err := s.hub.AppendEvent(ctx, ev); err != nil {
			s.logger.Warn("session: append skill_loaded", "err", err, "session", s.id)
		}
	}
	return nil
}

// UnloadSkill removes the skill + its references + its binding
// providers from state and the tools.Manager.
func (s *Session) UnloadSkill(ctx context.Context, name string) error {
	if !s.state.RemoveSkill(name) {
		return nil
	}
	s.state.RemoveRefsForSkill(name)
	s.dropBindings(ctx, name)
	s.touch()

	if s.hub != nil {
		meta, _ := json.Marshal(interfaces.SkillUnloadedMeta{Skill: name})
		ev := interfaces.SessionEvent{
			SessionID: s.id,
			AgentID:   s.hub.AgentID(),
			EventType: interfaces.EventTypeSkillUnloaded,
			Author:    s.id,
			Content:   name,
			Metadata:  jsonToMap(meta),
		}
		if _, err := s.hub.AppendEvent(ctx, ev); err != nil {
			s.logger.Warn("session: append skill_unloaded", "err", err, "session", s.id)
		}
	}
	return nil
}

// LoadReference appends the reference to the prompt (no hub persistence
// in 004 — references are re-derivable from the skill directory).
func (s *Session) LoadReference(ctx context.Context, skill, ref string) error {
	if _, err := s.skills.Reference(ctx, skill, ref); err != nil {
		return err
	}
	s.state.AddRef(skill, ref)
	s.touch()
	return nil
}

// UnloadReference drops a previously-loaded reference from the prompt.
func (s *Session) UnloadReference(_ context.Context, skill, ref string) error {
	if s.state.RemoveRef(skill, ref) {
		s.touch()
	}
	return nil
}

// Snapshot rebuilds the prompt + tool list on every call — skills may
// have been edited on disk and MCP tools may have changed server-side,
// both are reflected immediately.
func (s *Session) Snapshot() interfaces.Snapshot {
	ctx := context.Background()
	return interfaces.Snapshot{
		Prompt: s.buildPrompt(ctx),
		Tools:  s.buildTools(ctx),
	}
}

// IngestADKEvent bumps updatedAt — 003b will classify and persist.
func (s *Session) IngestADKEvent(_ context.Context, ev *adksession.Event) {
	if ev == nil {
		return
	}
	s.mu.Lock()
	s.updatedAt = ev.Timestamp
	s.mu.Unlock()
}

// ------------------------------------------------------------
// internal
// ------------------------------------------------------------

// bindSkillProvider ensures a FilteredProvider exists in tools.Manager
// for the skill's i-th provider spec. Named providers are resolved
// against the manager; inline endpoints are built via the session
// manager's InlineBuilder (if set).
func (s *Session) bindSkillProvider(skillName string, idx int, spec skills.SkillProviderSpec) error {
	binding := bindingName(skillName, idx)
	if _, err := s.tools.Provider(binding); err == nil {
		return nil // already bound
	}

	var raw tools.Provider
	switch {
	case spec.Provider != "":
		p, err := s.tools.Provider(spec.Provider)
		if err != nil {
			return fmt.Errorf("provider %q not registered", spec.Provider)
		}
		raw = p
	case spec.Endpoint != "":
		if s.manager.inlineBuilder == nil {
			return fmt.Errorf("inline endpoint provided but no InlineBuilder configured")
		}
		name := inlineName(skillName, idx)
		if existing, err := s.tools.Provider(name); err == nil {
			raw = existing
		} else {
			p, err := s.manager.inlineBuilder(name, spec.Endpoint, spec.Auth, s.logger)
			if err != nil {
				return fmt.Errorf("inline provider: %w", err)
			}
			s.tools.AddProvider(p)
			raw = p
		}
	default:
		return fmt.Errorf("spec has neither provider nor endpoint")
	}

	s.tools.AddProvider(tools.NewFiltered(binding, raw, spec.Tools))
	return nil
}

func (s *Session) buildPrompt(ctx context.Context) string {
	var b strings.Builder
	b.WriteString(s.constitution)

	s.state.mu.RLock()
	catalogOverride := s.state.CatalogSkills
	catalogSet := s.state.CatalogSet
	activeSkills := append([]string(nil), s.state.Skills...)
	refs := append([]string(nil), s.state.Refs...)
	s.state.mu.RUnlock()

	var catalog []interfaces.SkillMeta
	if catalogSet {
		catalog = catalogOverride
	} else if list, err := s.skills.List(ctx); err == nil {
		catalog = list
	}
	if text := s.skills.RenderCatalog(catalog); text != "" {
		b.WriteString("\n\n")
		b.WriteString(text)
	}

	loadedBySkill := map[string][]string{}
	for _, ref := range refs {
		skill, refName, ok := splitRef(ref)
		if !ok {
			continue
		}
		loadedBySkill[skill] = append(loadedBySkill[skill], refName)
	}

	for _, name := range activeSkills {
		sk, err := s.skills.Load(ctx, name)
		if err != nil {
			s.logger.Warn("session: load active skill", "skill", name, "err", err)
			continue
		}
		toolNames := s.skillToolNames(sk)
		text := skills.RenderInstructions(sk, toolNames, loadedBySkill[name])
		b.WriteString("\n\n")
		b.WriteString(text)
	}

	for _, ref := range refs {
		skill, refName, ok := splitRef(ref)
		if !ok {
			continue
		}
		content, err := s.skills.Reference(ctx, skill, refName)
		if err != nil {
			continue
		}
		b.WriteString("\n\n")
		b.WriteString(skills.RenderReference(refName, content))
	}

	return b.String()
}

func (s *Session) buildTools(ctx context.Context) []tool.Tool {
	s.state.mu.RLock()
	active := append([]string(nil), s.state.Skills...)
	s.state.mu.RUnlock()

	var out []tool.Tool
	seen := map[string]bool{}
	for _, skillName := range active {
		sk, err := s.skills.Load(ctx, skillName)
		if err != nil {
			continue
		}
		for i := range sk.Providers {
			ts, err := s.tools.ProviderTools(bindingName(skillName, i))
			if err != nil {
				continue
			}
			for _, t := range ts {
				if seen[t.Name()] {
					continue
				}
				seen[t.Name()] = true
				out = append(out, t)
			}
		}
	}
	return out
}

// skillToolNames returns the tool names currently exposed by a skill's
// provider bindings — used by the prompt builder to render the
// "Available tools" list alongside the skill instructions.
func (s *Session) skillToolNames(sk *skills.Skill) []string {
	var names []string
	seen := map[string]bool{}
	for i := range sk.Providers {
		ts, err := s.tools.ProviderTools(bindingName(sk.Name, i))
		if err != nil {
			continue
		}
		for _, t := range ts {
			if seen[t.Name()] {
				continue
			}
			seen[t.Name()] = true
			names = append(names, t.Name())
		}
	}
	return names
}

func (s *Session) touch() {
	s.mu.Lock()
	s.updatedAt = time.Now()
	s.mu.Unlock()
}

// dropBindings removes the FilteredProvider views + any inline raw
// providers that were registered for a skill, without touching
// pre-configured providers (hugr-main, _skills, etc). Safe to call on
// a skill whose on-disk definition has since changed shape — we reload
// the skill to inspect its provider list.
func (s *Session) dropBindings(ctx context.Context, name string) {
	sk, err := s.skills.Load(ctx, name)
	if err != nil {
		return
	}
	for i, spec := range sk.Providers {
		_ = s.tools.RemoveProvider(bindingName(name, i))
		if spec.Provider == "" {
			// inline raw provider synthesised in bindSkillProvider
			_ = s.tools.RemoveProvider(inlineName(name, i))
		}
	}
}

// dropAllBindings clears every skill's bindings at once — used by
// Manager.Delete so closing a session leaves no orphaned FilteredProvider
// views in tools.Manager.
func (s *Session) dropAllBindings(ctx context.Context) {
	s.state.mu.RLock()
	active := append([]string(nil), s.state.Skills...)
	s.state.mu.RUnlock()
	for _, name := range active {
		s.dropBindings(ctx, name)
	}
}

// restoreSkill re-applies a persisted skill_loaded event during
// Manager.RestoreOpen — goes through the normal LoadSkill path minus
// the hub event write (that's already on disk).
func (s *Session) restoreSkill(ctx context.Context, name string) {
	sk, err := s.skills.Load(ctx, name)
	if err != nil {
		s.logger.Warn("session: restore skill missing", "skill", name, "err", err)
		return
	}
	s.state.AddSkill(sk.Name)
	for i, spec := range sk.Providers {
		if err := s.bindSkillProvider(sk.Name, i, spec); err != nil {
			s.logger.Warn("session: bind on restore", "skill", sk.Name, "idx", i, "err", err)
		}
	}
	s.touch()
}

// ------------------------------------------------------------
// helpers
// ------------------------------------------------------------

func splitRef(s string) (skill, ref string, ok bool) {
	i := strings.IndexByte(s, '/')
	if i <= 0 || i >= len(s)-1 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

func jsonToMap(b []byte) map[string]any {
	if len(b) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}

// bindingName is the tools.Manager key for a skill's i-th provider
// binding. Stable across runs so idempotent LoadSkill works.
func bindingName(skill string, idx int) string {
	return fmt.Sprintf("skill/%s/%d", skill, idx)
}

// inlineName is the tools.Manager key for the synthetic raw provider
// backing a skill's inline endpoint binding (i.e. when the spec had
// no provider: reference).
func inlineName(skill string, idx int) string {
	return fmt.Sprintf("inline/%s/%d", skill, idx)
}

// Touch is for tests/infra that need to flag activity.
func (s *Session) Touch() { s.touch() }

// ListSkills returns the current skill catalogue (system.catalogLister).
func (s *Session) ListSkills(ctx context.Context) ([]interfaces.SkillMeta, error) {
	return s.skills.List(ctx)
}

// SkillMeta returns the reference-document metadata + next-step hint for
// a skill (system.skillDescriptor). Empty struct if the skill is unknown.
func (s *Session) SkillMeta(ctx context.Context, name string) system.SkillDescriptorMeta {
	sk, err := s.skills.Load(ctx, name)
	if err != nil {
		return system.SkillDescriptorMeta{}
	}
	return system.SkillDescriptorMeta{
		Refs:     append([]interfaces.SkillRefMeta(nil), sk.Refs...),
		NextStep: sk.NextStep,
	}
}

// ReadReference returns the raw content of a skill reference document
// (system.refReader).
func (s *Session) ReadReference(ctx context.Context, skill, ref string) (string, error) {
	return s.skills.Reference(ctx, skill, ref)
}
