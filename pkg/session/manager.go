package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hugr-lab/hugen/interfaces"
	"github.com/hugr-lab/hugen/pkg/skills"
	"github.com/hugr-lab/hugen/pkg/tools"
	adksession "google.golang.org/adk/session"
)

// Config bundles Manager dependencies.
type Config struct {
	// Skills catalogue.
	Skills skills.Manager

	// Tools manager. Providers (including system suites) are expected
	// to have been registered in cmd/agent before New is called —
	// Manager itself no longer contributes a provider.
	Tools *tools.Manager

	// Hub persists session rows and skill lifecycle events. May be nil
	// for tests.
	Hub interfaces.HubDB

	// Constitution is the base system prompt text.
	Constitution string

	// InlineBuilder is called by LoadSkill when a skill declares an
	// inline MCP endpoint (not a named Provider reference). It builds
	// a tools.Provider under the given synthetic name, typically by
	// delegating to providers.Build with type=mcp. May be nil — skills
	// that only use named providers work without it.
	InlineBuilder InlineProviderFactory

	// Logger may be nil; defaults to slog.Default.
	Logger *slog.Logger
}

// InlineProviderFactory builds an anonymous MCP provider for a skill's
// inline endpoint spec. Called at most once per distinct
// (skillName, endpoint, auth) combination — the resulting provider is
// registered in tools.Manager under the provided synthetic name.
type InlineProviderFactory func(name, endpoint, authName string, logger *slog.Logger) (tools.Provider, error)

// Manager owns runtime sessions. Implements adksession.Service and
// interfaces.SessionManager. System tools no longer live here — they
// come from a tools.Provider declared in config.yaml (`type: system`,
// `suite: skills`).
type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*Session // sessionID → session

	skills        skills.Manager
	tools         *tools.Manager
	hub           interfaces.HubDB
	constitution  string
	logger        *slog.Logger
	inlineBuilder InlineProviderFactory
}

var (
	_ adksession.Service        = (*Manager)(nil)
	_ interfaces.SessionManager = (*Manager)(nil)
)

// New builds a Manager.
func New(cfg Config) *Manager {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Manager{
		sessions:      make(map[string]*Session),
		skills:        cfg.Skills,
		tools:         cfg.Tools,
		hub:           cfg.Hub,
		constitution:  cfg.Constitution,
		logger:        cfg.Logger,
		inlineBuilder: cfg.InlineBuilder,
	}
}

// ------------------------------------------------------------
// interfaces.SessionManager
// ------------------------------------------------------------

// Session returns the runtime session matching id, or an error.
func (m *Manager) Session(id string) (interfaces.Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session: %q not tracked", id)
	}
	return s, nil
}

// Cleanup removes sessions inactive for more than olderThan.
func (m *Manager) Cleanup(olderThan time.Duration) int {
	cutoff := time.Now().Add(-olderThan)
	m.mu.Lock()
	defer m.mu.Unlock()
	removed := 0
	for id, s := range m.sessions {
		if s.LastUpdateTime().Before(cutoff) {
			delete(m.sessions, id)
			removed++
		}
	}
	return removed
}

// RestoreOpen re-creates Session objects from hub.db for every row with
// status="active" and replays their skill-lifecycle events. Autoload
// skills are applied afterwards to top up the session in case new
// autoload entries were added since the session last ran.
func (m *Manager) RestoreOpen(ctx context.Context) error {
	if m.hub == nil {
		return nil
	}
	rows, err := m.hub.ListActiveSessions(ctx)
	if err != nil {
		return fmt.Errorf("session: list active: %w", err)
	}
	for _, row := range rows {
		events, err := m.hub.GetEvents(ctx, row.ID)
		if err != nil {
			m.logger.Warn("session: get events", "id", row.ID, "err", err)
			continue
		}
		active := map[string]struct{}{}
		for _, ev := range events {
			switch ev.EventType {
			case interfaces.EventTypeSkillLoaded:
				name := ev.Content
				if name == "" {
					var meta interfaces.SkillLoadedMeta
					raw, _ := json.Marshal(ev.Metadata)
					_ = json.Unmarshal(raw, &meta)
					name = meta.Skill
				}
				if name != "" {
					active[name] = struct{}{}
				}
			case interfaces.EventTypeSkillUnloaded:
				delete(active, ev.Content)
			}
		}

		sess := m.newLocal(row.ID, "", row.OwnerID)
		for name := range active {
			sess.restoreSkill(ctx, name)
		}
		m.applyAutoload(ctx, sess)

		m.mu.Lock()
		m.sessions[row.ID] = sess
		m.mu.Unlock()
	}
	m.logger.Info("session: restored", "count", len(rows))
	return nil
}

// applyAutoload loads every skill marked autoload into sess, idempotent
// against AddSkill so repeat calls are safe.
func (m *Manager) applyAutoload(ctx context.Context, sess *Session) {
	if m.skills == nil {
		return
	}
	names, err := m.skills.AutoloadNames(ctx)
	if err != nil {
		m.logger.Warn("session: autoload lookup", "err", err)
		return
	}
	for _, name := range names {
		if err := sess.LoadSkill(ctx, name); err != nil {
			m.logger.Warn("session: autoload", "skill", name, "err", err)
		}
	}
}

// ------------------------------------------------------------
// adksession.Service
// ------------------------------------------------------------

func (m *Manager) Create(ctx context.Context, req *adksession.CreateRequest) (*adksession.CreateResponse, error) {
	if req == nil {
		return nil, errors.New("session: Create: nil request")
	}
	if req.AppName == "" || req.UserID == "" {
		return nil, fmt.Errorf("session: Create: app_name and user_id required (got %q / %q)", req.AppName, req.UserID)
	}
	id := req.SessionID
	if id == "" {
		id = uuid.NewString()
	}

	m.mu.Lock()
	if _, exists := m.sessions[id]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("session: %q already exists", id)
	}
	sess := m.newLocal(id, req.AppName, req.UserID)
	m.sessions[id] = sess
	m.mu.Unlock()

	if req.State != nil {
		for k, v := range req.State {
			_ = sess.state.Set(k, v)
		}
	}

	if m.hub != nil {
		row := interfaces.SessionRecord{
			ID:      id,
			AgentID: m.hub.AgentID(),
			OwnerID: req.UserID,
			Status:  "active",
		}
		if _, err := m.hub.CreateSession(ctx, row); err != nil {
			m.logger.Warn("session: hub.CreateSession", "id", id, "err", err)
		}
	}

	m.applyAutoload(ctx, sess)

	return &adksession.CreateResponse{Session: sess}, nil
}

func (m *Manager) Get(ctx context.Context, req *adksession.GetRequest) (*adksession.GetResponse, error) {
	if req == nil || req.SessionID == "" {
		return nil, errors.New("session: Get: session_id required")
	}
	m.mu.RLock()
	sess, ok := m.sessions[req.SessionID]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("session: %q not found", req.SessionID)
	}
	return &adksession.GetResponse{Session: sess}, nil
}

func (m *Manager) List(ctx context.Context, req *adksession.ListRequest) (*adksession.ListResponse, error) {
	if req == nil {
		return nil, errors.New("session: List: nil request")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]adksession.Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		if req.AppName != "" && s.appName != req.AppName {
			continue
		}
		if req.UserID != "" && s.userID != req.UserID {
			continue
		}
		out = append(out, s)
	}
	return &adksession.ListResponse{Sessions: out}, nil
}

func (m *Manager) Delete(ctx context.Context, req *adksession.DeleteRequest) error {
	if req == nil || req.SessionID == "" {
		return errors.New("session: Delete: session_id required")
	}
	m.mu.Lock()
	delete(m.sessions, req.SessionID)
	m.mu.Unlock()

	if m.hub != nil {
		if err := m.hub.UpdateSessionStatus(ctx, req.SessionID, "closed"); err != nil {
			m.logger.Warn("session: hub.UpdateSessionStatus", "id", req.SessionID, "err", err)
		}
	}
	return nil
}

func (m *Manager) AppendEvent(ctx context.Context, cur adksession.Session, ev *adksession.Event) error {
	if cur == nil {
		return errors.New("session: AppendEvent: nil session")
	}
	if ev == nil {
		return errors.New("session: AppendEvent: nil event")
	}
	if ev.Partial {
		return nil
	}
	sess, ok := cur.(*Session)
	if !ok {
		return fmt.Errorf("session: AppendEvent: unexpected session type %T", cur)
	}

	sess.events.append(ev)
	if len(ev.Actions.StateDelta) > 0 {
		for k, v := range ev.Actions.StateDelta {
			_ = sess.state.Set(k, v)
		}
	}
	sess.IngestADKEvent(ctx, ev)
	return nil
}

// ------------------------------------------------------------
// internal
// ------------------------------------------------------------

func (m *Manager) newLocal(id, app, user string) *Session {
	return newSession(sessionConfig{
		id:           id,
		appName:      app,
		userID:       user,
		manager:      m,
		skills:       m.skills,
		tools:        m.tools,
		hub:          m.hub,
		logger:       m.logger,
		constitution: m.constitution,
	})
}
