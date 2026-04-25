package sessions

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/hugr-lab/hugen/pkg/skills"
	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
	"github.com/hugr-lab/hugen/pkg/tools"
	adksession "google.golang.org/adk/session"
	"google.golang.org/adk/tool"
)

// Session is a runtime conversation. It implements adksession.Session and
// *Session. Prompt/tools are resolved at Snapshot time against
// the current skills.Manager and tools.Manager — no caching, so hot-edits
// of skills or newly-registered MCP tools are visible on the next turn.
type Session struct {
	id      string
	appName string
	userID  string
	// sessionType mirrors sessions.session_type in hub (spec 006). Used
	// by Manager.applyAutoload to filter autoload skills against the
	// session's discriminator: a "subagent" session only picks up
	// autoload skills whose AutoloadFor includes "subagent". Empty
	// value at runtime is treated as SessionTypeRoot (defensive).
	sessionType        string
	parentSessionID    string
	spawnedFromEventID string
	mission            string
	forkAfterSeq       *int
	// Cached sub-agent identity sourced from sessions.metadata at
	// Create / restore time (spec 006 §6). Used by renderNotesBlock to
	// prefix cross-session notes as "[from <skill>/<role>]" without
	// paying a hub round-trip per render.
	metaSkill string
	metaRole  string

	state  *State
	events *eventStore

	manager *Manager
	skills  skills.Manager
	tools   *tools.Manager
	hub     *sessstore.Client
	logger  *slog.Logger

	constitution string

	mu           sync.RWMutex
	updatedAt    time.Time
	notesCache   string    // rendered "## Session notes" block
	notesCacheAt time.Time // render time for 10s TTL

	// writeMu serialises every (nextSeq → INSERT) pair on this
	// session's transcript. Synchronous writers (LoadSkill,
	// UnloadSkill, compactor events through Manager.AppendEvent) and
	// the async classifier all funnel through AppendEvent, which
	// holds this lock across the store call — so a write can't read a
	// stale max(seq) while another is still committing.
	writeMu sync.Mutex

	// Materialization: sessions restored from hub.db on startup are
	// created as stubs (no events, no bindings). The first Get hits
	// ensureMaterialized, which replays skill state + conversation
	// events exactly once. Fresh sessions from Create skip this via
	// markMaterialized.
	mzOnce sync.Once
	mzErr  error
}

// SessionType returns the session's discriminator
// (sessstore.SessionTypeRoot|SubAgent|Fork). Defaults to "root"
// for sessions created without an explicit type (preserves pre-006
// behaviour).
func (s *Session) SessionType() string {
	if s == nil || s.sessionType == "" {
		return sessstore.SessionTypeRoot
	}
	return s.sessionType
}

// ParentSessionID returns the linked parent session id (empty for
// root sessions).
func (s *Session) ParentSessionID() string {
	if s == nil {
		return ""
	}
	return s.parentSessionID
}

// SpawnedFromEventID returns the parent's tool_call event id that
// dispatched this session (empty for root sessions and pre-spec-006
// rows).
func (s *Session) SpawnedFromEventID() string {
	if s == nil {
		return ""
	}
	return s.spawnedFromEventID
}

// Mission returns the short task description recorded for this
// sub-agent session ("" for root sessions).
func (s *Session) Mission() string {
	if s == nil {
		return ""
	}
	return s.mission
}

var (
	_ adksession.Session = (*Session)(nil)
	_ *Session           = (*Session)(nil)
)

type sessionConfig struct {
	id           string
	appName      string
	userID       string
	manager      *Manager
	skills       skills.Manager
	tools        *tools.Manager
	hub          *sessstore.Client
	logger       *slog.Logger
	constitution string

	// Spec 006 sub-agent linkage. All optional — root sessions leave
	// these empty / nil; the dispatcher populates them when opening a
	// child session, and RestoreOpen copies them off the hub Record.
	sessionType        string
	parentSessionID    string
	spawnedFromEventID string
	mission            string
	forkAfterSeq       *int
	metaSkill          string
	metaRole           string
}

func newSession(cfg sessionConfig) *Session {
	return &Session{
		id:                 cfg.id,
		appName:            cfg.appName,
		userID:             cfg.userID,
		sessionType:        cfg.sessionType,
		parentSessionID:    cfg.parentSessionID,
		spawnedFromEventID: cfg.spawnedFromEventID,
		mission:            cfg.mission,
		forkAfterSeq:       cfg.forkAfterSeq,
		metaSkill:          cfg.metaSkill,
		metaRole:           cfg.metaRole,
		state:              NewState(),
		events:             newEventStore(),
		manager:            cfg.manager,
		skills:             cfg.skills,
		tools:              cfg.tools,
		hub:                cfg.hub,
		logger:             cfg.logger,
		constitution:       cfg.constitution,
		updatedAt:          time.Now(),
	}
}

// markMaterialized is called on freshly Created sessions so the
// first Get doesn't re-materialize from hub (which would no-op at
// best, re-append events at worst).
func (s *Session) markMaterialized() {
	s.mzOnce.Do(func() {})
}

// ensureMaterialized is called by Manager.Get before returning a
// Session stub restored at startup. Replays skill state + every
// conversation event from hub.db. Idempotent via sync.Once.
func (s *Session) ensureMaterialized(ctx context.Context) error {
	s.mzOnce.Do(func() {
		s.mzErr = s.materialize(ctx)
	})
	return s.mzErr
}

func (s *Session) materialize(ctx context.Context) error {
	if s.hub == nil {
		s.manager.applyAutoload(ctx, s)
		return nil
	}
	events, err := s.hub.GetEvents(ctx, s.id)
	if err != nil {
		return fmt.Errorf("materialize %q: get events: %w", s.id, err)
	}

	// Skills first: walk the log to figure out which skills ended up
	// active, then bind them. AddSkill is idempotent so double-loads
	// from autoload below are safe.
	active := replaySkillState(events)
	for name := range active {
		s.restoreSkill(ctx, name)
	}
	s.manager.applyAutoload(ctx, s)

	// Conversation events: push into the ADK event store so the LLM
	// sees the prior history on the next turn.
	for _, ev := range events {
		if adkEv, ok := convertToADKEvent(ev); ok {
			s.events.append(adkEv)
		}
	}
	s.touch()
	return nil
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
// *Session
// ------------------------------------------------------------

// SetCatalog replaces the catalog shown in this session's prompt.
func (s *Session) SetCatalog(list []skills.SkillMeta) error {
	s.state.SetCatalog(list)
	s.touch()
	return nil
}

// LoadSkill marks the skill active and registers a FilteredProvider
// per declared SkillProviderSpec. Bindings are keyed by provider
// name (`skill/<skill>/<providerName>`) so repeated LoadSkill calls
// are idempotent and provider names act as stable handles.
//
// Version drift: when skill.Version differs from the last-loaded
// version recorded in state, the old bindings (and any inline raw
// providers they owned) are torn down first. This lets operators
// edit a skill's provider list on disk, bump version:, and have the
// change applied without restarting the process.
//
// Atomicity: we bind every provider FIRST. If any binding fails we
// abort before touching state or hub, so a partial load doesn't leave
// a stale skill_loaded event on disk pointing at a skill with no tools.
func (s *Session) LoadSkill(ctx context.Context, name string) error {
	sk, err := s.skills.Load(ctx, name)
	if err != nil {
		return err
	}

	// Version drift → drop every old binding (filtered + inline raw)
	// before rebuilding. We sweep by prefix because the previously
	// bound provider names are not recoverable from the new skill
	// file's provider list (which has already changed). On first load
	// oldVer is absent and the sweep is a no-op.
	if oldVer, ok := s.state.LoadedSkillVersion(sk.Name); ok && oldVer != sk.Version {
		s.sweepSkillBindings(sk.Name)
		s.state.ForgetSkillVersion(sk.Name)
		s.logger.Info("skill version changed — bindings rebuilt",
			"skill", sk.Name, "old", oldVer, "new", sk.Version)
	}

	// Bind bindings first (phase 1). Roll back any that were created
	// in this call if a later one fails.
	created := make([]skills.SkillProviderSpec, 0, len(sk.Providers))
	for _, spec := range sk.Providers {
		existed := false
		if _, err := s.tools.Provider(bindingName(sk.Name, spec.Name)); err == nil {
			existed = true
		}
		if err := s.bindSkillProvider(sk.Name, spec); err != nil {
			// Roll back: drop only the providers we added in this call,
			// not ones already present from a prior successful load.
			for _, j := range created {
				_ = s.tools.RemoveProvider(bindingName(sk.Name, j.Name))
				if j.Provider == "" {
					_ = s.tools.RemoveProvider(inlineName(sk.Name, j.Name))
				}
			}
			return fmt.Errorf("load skill %q: provider %q: %w", sk.Name, spec.Name, err)
		}
		if !existed {
			created = append(created, spec)
		}
	}

	// Spec 006 T105: if the skill declares sub_agents and the manager
	// wired a builder, register one specific tool per role under a
	// dedicated provider. Non-fatal — a builder miss still leaves the
	// generic `subagent_dispatch` tool available via `_system`.
	s.bindSubAgents(sk)

	s.state.RecordSkillVersion(sk.Name, sk.Version)

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
			providerNames = append(providerNames, spec.Name)
		}
		meta, _ := json.Marshal(sessstore.SkillLoadedMeta{
			Skill: sk.Name,
			Tools: providerNames,
		})
		ev := sessstore.Event{
			SessionID: s.id,
			AgentID:   s.hub.AgentID(),
			EventType: sessstore.EventTypeSkillLoaded,
			Author:    s.id,
			Content:   sk.Name,
			Metadata:  jsonToMap(meta),
		}
		if _, err := s.AppendEvent(ctx, ev, ""); err != nil {
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
	s.state.ForgetSkillVersion(name)
	s.dropBindings(ctx, name)
	s.touch()

	if s.hub != nil {
		meta, _ := json.Marshal(sessstore.SkillUnloadedMeta{Skill: name})
		ev := sessstore.Event{
			SessionID: s.id,
			AgentID:   s.hub.AgentID(),
			EventType: sessstore.EventTypeSkillUnloaded,
			Author:    s.id,
			Content:   name,
			Metadata:  jsonToMap(meta),
		}
		if _, err := s.AppendEvent(ctx, ev, ""); err != nil {
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
func (s *Session) Snapshot() Snapshot {
	ctx := context.Background()
	return Snapshot{
		Prompt: s.buildPrompt(ctx),
		Tools:  s.buildTools(ctx),
	}
}

// IngestADKEvent bumps updatedAt and, if the manager has a classifier
// attached, pushes the event onto the classifier channel for
// asynchronous persistence to hub.db.session_events. Non-blocking —
// a full classifier queue drops the event (WARN) so the LLM turn is
// never stalled on DB I/O (spec 005 FR-001, SC-009).
func (s *Session) IngestADKEvent(_ context.Context, ev *adksession.Event) {
	if ev == nil {
		return
	}
	s.mu.Lock()
	if !ev.Timestamp.IsZero() {
		s.updatedAt = ev.Timestamp
	} else {
		s.updatedAt = time.Now()
	}
	s.mu.Unlock()
	if s.manager != nil {
		s.manager.publishEvent(s.id, ev)
	}
}

// ------------------------------------------------------------
// internal
// ------------------------------------------------------------

// bindSkillProvider ensures a FilteredProvider exists in tools.Manager
// for the skill's provider spec. Named providers are resolved against
// the manager; inline endpoints are built via the session manager's
// InlineBuilder (if set). Binding key uses spec.Name so providers
// across skills with the same logical name resolve consistently.
func (s *Session) bindSkillProvider(skillName string, spec skills.SkillProviderSpec) error {
	if spec.Name == "" {
		return fmt.Errorf("provider spec has empty name")
	}
	binding := bindingName(skillName, spec.Name)
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
		name := inlineName(skillName, spec.Name)
		if existing, err := s.tools.Provider(name); err == nil {
			raw = existing
		} else {
			authIn := InlineProviderAuth{
				Type:        spec.AuthType,
				Name:        spec.Auth,
				HeaderName:  spec.AuthHeaderName,
				HeaderValue: spec.AuthHeaderValue,
			}
			p, err := s.manager.inlineBuilder(name, spec.Endpoint, authIn, s.logger)
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

	// US1 snapshot contract (spec 006 §3a): sub-agent sessions carry
	// only the parent skill's body — no full catalogue. The specialist
	// operates on a single mission; exposing every available skill
	// would invite it to re-plan outside its lane and burn turns.
	// Root (and fork) sessions keep the full catalogue.
	if s.sessionType != sessstore.SessionTypeSubAgent {
		var catalog []skills.SkillMeta
		if catalogSet {
			catalog = catalogOverride
		} else if list, err := s.skills.List(ctx); err == nil {
			catalog = list
		}
		if text := s.skills.RenderCatalog(catalog); text != "" {
			b.WriteString("\n\n")
			b.WriteString(text)
		}
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

	if block := s.renderNotesBlock(ctx); block != "" {
		b.WriteString("\n\n")
		b.WriteString(block)
	}

	return b.String()
}

// renderNotesBlock pulls the session's notes chain from HubDB and
// renders a fixed-part "## Session notes" block that survives context
// compaction. Spec 006 §6: the block sources from the
// session_notes_chain view so a coordinator sees notes its specialists
// promoted via memory_note(scope: "parent" | "ancestors"). Notes
// authored by another session (chain promotions) render with a
// "[from <skill>/<role>]" prefix using the author session's metadata.
// Cached at the session level for 10 s; invalidated explicitly by the
// memory_note / memory_clear_note tools.
func (s *Session) renderNotesBlock(ctx context.Context) string {
	if s.hub == nil {
		return ""
	}
	s.mu.RLock()
	cached := s.notesCache
	cachedAt := s.notesCacheAt
	s.mu.RUnlock()
	if cachedAt.After(time.Now().Add(-10*time.Second)) && cached != "" {
		return cached
	}
	notes, err := s.hub.ListNotesChain(ctx, s.id)
	if err != nil || len(notes) == 0 {
		s.mu.Lock()
		s.notesCache = ""
		s.notesCacheAt = time.Now()
		s.mu.Unlock()
		return ""
	}
	// Cache author-session metadata for the duration of this render so
	// a coordinator with ten notes from one specialist only pays one
	// hub round-trip.
	authorMeta := map[string]string{}
	var b strings.Builder
	b.WriteString("## Session notes\n")
	for _, n := range notes {
		b.WriteString("- [")
		b.WriteString(n.ID)
		b.WriteString("] ")
		if n.AuthorSessionID != "" && n.AuthorSessionID != s.id {
			prefix, ok := authorMeta[n.AuthorSessionID]
			if !ok {
				prefix = s.resolveAuthorPrefix(ctx, n.AuthorSessionID)
				authorMeta[n.AuthorSessionID] = prefix
			}
			if prefix != "" {
				b.WriteString(prefix)
				b.WriteString(" ")
			}
		}
		b.WriteString(n.Content)
		b.WriteString("\n")
	}
	out := b.String()
	s.mu.Lock()
	s.notesCache = out
	s.notesCacheAt = time.Now()
	s.mu.Unlock()
	return out
}

// resolveAuthorPrefix looks up the skill/role pair for a cross-session
// note's author and returns the "[from <skill>/<role>]" tag (or an
// empty string when metadata is missing). Best-effort: metadata lookup
// failures silently degrade to no prefix rather than break rendering.
func (s *Session) resolveAuthorPrefix(ctx context.Context, authorID string) string {
	// In-memory first — specialists dispatched in this process are
	// tracked by the Manager and carry session metadata. Falling back
	// to hub works for long-gone authors the Manager has evicted.
	var skill, role string
	if s.manager != nil {
		if peer, err := s.manager.Session(authorID); err == nil && peer != nil {
			skill, role = peer.metadataSkillRole()
		}
	}
	if skill == "" && role == "" && s.hub != nil {
		if rec, err := s.hub.GetSession(ctx, authorID); err == nil && rec != nil {
			if v, ok := rec.Metadata["skill"].(string); ok {
				skill = v
			}
			if v, ok := rec.Metadata["role"].(string); ok {
				role = v
			}
		}
	}
	switch {
	case skill != "" && role != "":
		return "[from " + skill + "/" + role + "]"
	case skill != "":
		return "[from " + skill + "]"
	case role != "":
		return "[from " + role + "]"
	default:
		return ""
	}
}

// metadataSkillRole returns the skill/role pair cached on the Session
// at Create / restore time. Returns empty strings when the session did
// not carry skill/role metadata (i.e. coordinator / fork sessions).
func (s *Session) metadataSkillRole() (string, string) {
	if s == nil {
		return "", ""
	}
	return s.metaSkill, s.metaRole
}

// Skill returns the (skill, role) the sub-agent session was dispatched
// under, or empty strings when the session is a coordinator / fork.
// Read-only; cached at Create + restore. Used by spec-007's
// spawn_sub_mission tool to enforce the can_spawn / max_depth gates
// without re-reading hub.db.
func (s *Session) Skill() string {
	if s == nil {
		return ""
	}
	return s.metaSkill
}

func (s *Session) Role() string {
	if s == nil {
		return ""
	}
	return s.metaRole
}

// AppendEvent writes a transcript event to the hub for this session
// with mutual exclusion against every other writer on the same
// session. Call this from any path (synchronous skill events,
// compactor, classifier-facing Manager.AppendEvent) that would
// otherwise race on nextSeq. Empty SessionID on ev is filled in from
// the session's own id.
//
// Returns (id, nil) on success and ("", err) on store failure. When
// the session has no hub (test harness), returns ("", nil) without
// touching the lock.
func (s *Session) AppendEvent(ctx context.Context, ev sessstore.Event, summary string) (string, error) {
	if s == nil || s.hub == nil {
		return "", nil
	}
	if ev.SessionID == "" {
		ev.SessionID = s.id
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.hub.AppendEventWithSummary(ctx, ev, summary)
}

// InvalidateNotesCache clears the session's notes-render cache so the
// next Snapshot re-reads notes from HubDB. Called by the memory_note
// and memory_clear_note tools after they mutate.
func (s *Session) InvalidateNotesCache() {
	s.mu.Lock()
	s.notesCache = ""
	s.notesCacheAt = time.Time{}
	s.mu.Unlock()
}

func (s *Session) buildTools(ctx context.Context) []tool.Tool {
	s.state.mu.RLock()
	active := append([]string(nil), s.state.Skills...)
	s.state.mu.RUnlock()

	var out []tool.Tool
	seen := map[string]bool{}
	collect := func(providerKey string) {
		ts, err := s.tools.ProviderTools(providerKey)
		if err != nil {
			return
		}
		for _, t := range ts {
			if seen[t.Name()] {
				continue
			}
			seen[t.Name()] = true
			out = append(out, t)
		}
	}
	for _, skillName := range active {
		sk, err := s.skills.Load(ctx, skillName)
		if err != nil {
			continue
		}
		for _, spec := range sk.Providers {
			collect(bindingName(skillName, spec.Name))
		}
		// Spec 006 T105: pick up the per-skill _subagents provider
		// when the skill declares sub_agents. Registration happens in
		// bindSubAgents; no-op when absent.
		if len(sk.SubAgents) > 0 {
			collect(subagentsName(sk.Name))
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
	for _, spec := range sk.Providers {
		ts, err := s.tools.ProviderTools(bindingName(sk.Name, spec.Name))
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
// the skill to inspect its provider list. When the skill file is gone
// (or unparseable) we fall back to sweeping every skill/<name>/* and
// skill-inline/<name>/* prefix so orphaned bindings still get cleaned.
func (s *Session) dropBindings(ctx context.Context, name string) {
	sk, err := s.skills.Load(ctx, name)
	if err != nil {
		s.sweepSkillBindings(name)
		return
	}
	for _, spec := range sk.Providers {
		_ = s.tools.RemoveProvider(bindingName(name, spec.Name))
		if spec.Provider == "" {
			// inline raw provider synthesised in bindSkillProvider
			_ = s.tools.RemoveProvider(inlineName(name, spec.Name))
		}
	}
	// T105: also drop the per-skill sub-agent tool provider. Safe to
	// call unconditionally — RemoveProvider on a missing key is a no-op.
	_ = s.tools.RemoveProvider(subagentsName(name))
}

// sweepSkillBindings removes every registered provider whose name
// starts with "skill/<skillName>/" or "skill-inline/<skillName>/".
// Used as a fallback when the on-disk skill has gone missing but we
// still need to tear its runtime bindings down.
func (s *Session) sweepSkillBindings(skillName string) {
	prefixes := []string{
		bindingName(skillName, ""),
		inlineName(skillName, ""),
	}
	for _, p := range prefixes {
		for _, existing := range s.tools.ProviderNames() {
			if strings.HasPrefix(existing, p) {
				_ = s.tools.RemoveProvider(existing)
			}
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
	for _, spec := range sk.Providers {
		if err := s.bindSkillProvider(sk.Name, spec); err != nil {
			s.logger.Warn("session: bind on restore",
				"skill", sk.Name, "provider", spec.Name, "err", err)
		}
	}
	s.state.RecordSkillVersion(sk.Name, sk.Version)
	s.bindSubAgents(sk)
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

// bindingName is the tools.Manager key for a skill's provider-scope
// filtered binding. Stable across runs so idempotent LoadSkill works.
// When providerName is empty the result is the sweep prefix used by
// sweepSkillBindings.
func bindingName(skill, providerName string) string {
	return fmt.Sprintf("skill/%s/%s", skill, providerName)
}

// inlineName is the tools.Manager key for the synthetic raw provider
// backing a skill's inline endpoint binding (i.e. when the spec had
// no provider: reference). Prefix ensures two skills declaring the
// same inline provider name don't collide — each owns its own
// skill-inline/<skill>/<providerName> entry.
func inlineName(skill, providerName string) string {
	return fmt.Sprintf("skill-inline/%s/%s", skill, providerName)
}

// subagentsName is the tools.Manager key for the per-skill
// sub-agent tool provider registered when a skill declares
// non-empty sub_agents. One entry per role lives inside it
// (Name: subagent_<skill>_<role>). Detached by dropBindings.
func subagentsName(skill string) string {
	return fmt.Sprintf("skill/%s/_subagents", skill)
}

// bindSubAgents registers one tool.Tool per role declared under the
// skill's `sub_agents:` frontmatter block (name
// `subagent_<skill>_<role>`, args {task, notes?}). Provider name
// `skill/<skill>/_subagents` mirrors the provider-binding convention
// so dropBindings / sweepSkillBindings clean it up on unload /
// version-drift automatically.
//
// No-op when the manager didn't supply a builder (pkg/sessions stays
// agent-agnostic) or when the skill has no sub_agents.
func (s *Session) bindSubAgents(sk *skills.Skill) {
	if s == nil || s.manager == nil || s.manager.subagentBuilder == nil {
		return
	}
	if len(sk.SubAgents) == 0 {
		return
	}
	tools := make([]tool.Tool, 0, len(sk.SubAgents))
	for role, spec := range sk.SubAgents {
		t := s.manager.subagentBuilder(sk.Name, role, spec)
		if t == nil {
			continue
		}
		tools = append(tools, t)
	}
	if len(tools) == 0 {
		return
	}
	s.tools.AddProvider(subagentsProvider{
		name:  subagentsName(sk.Name),
		tools: tools,
	})
}

// subagentsProvider is a minimal tools.Provider wrapping a static
// tool list — purely a sessions-package construct so we don't need
// a public constructor in pkg/tools for this one call site.
type subagentsProvider struct {
	name  string
	tools []tool.Tool
}

func (p subagentsProvider) Name() string       { return p.name }
func (p subagentsProvider) Tools() []tool.Tool { return p.tools }

// Touch is for tests/infra that need to flag activity.
func (s *Session) Touch() { s.touch() }

// ActiveSkills returns the names of skills currently loaded on this
// session. Snapshot is the single source of truth for what the LLM
// sees; this accessor lets tool handlers make short decisions
// (is skill X loaded?) without re-rendering the snapshot.
func (s *Session) ActiveSkills() []string {
	if s == nil || s.state == nil {
		return nil
	}
	s.state.mu.RLock()
	defer s.state.mu.RUnlock()
	if len(s.state.Skills) == 0 {
		return nil
	}
	out := make([]string, len(s.state.Skills))
	copy(out, s.state.Skills)
	return out
}

// HasSkill reports whether the named skill is currently loaded on
// this session. Used by the subagent_dispatch tool to refuse
// dispatches into skills that weren't loaded by the coordinator.
func (s *Session) HasSkill(name string) bool {
	if s == nil || s.state == nil || name == "" {
		return false
	}
	s.state.mu.RLock()
	defer s.state.mu.RUnlock()
	for _, n := range s.state.Skills {
		if n == name {
			return true
		}
	}
	return false
}

// SkillDescriptorMeta is what skill_load needs to shape its response —
// references with descriptions + the author-provided workflow hint.
// ListSkills returns the current skill catalogue.
func (s *Session) ListSkills(ctx context.Context) ([]skills.SkillMeta, error) {
	return s.skills.List(ctx)
}

// SkillMeta returns the reference-document metadata + next-step hint for
// a skill. Empty struct if the skill is unknown.
func (s *Session) SkillMeta(ctx context.Context, name string) skills.DescriptorMeta {
	sk, err := s.skills.Load(ctx, name)
	if err != nil {
		return skills.DescriptorMeta{}
	}
	return skills.DescriptorMeta{
		Refs:     append([]skills.SkillRefMeta(nil), sk.Refs...),
		NextStep: sk.NextStep,
	}
}

// ReadReference returns the raw content of a skill reference document
// (system.refReader).
func (s *Session) ReadReference(ctx context.Context, skill, ref string) (string, error) {
	return s.skills.Reference(ctx, skill, ref)
}
