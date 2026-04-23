package sessions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hugr-lab/hugen/pkg/skills"
	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
	"github.com/hugr-lab/hugen/pkg/tools"
	"github.com/hugr-lab/query-engine/types"
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

	// Querier is used by the Manager to build its own sessstore client
	// internally when Sessions is nil. May be nil for tests that drive
	// Session construction manually — in that case no hub persistence
	// happens.
	Querier types.Querier

	// Sessions is a pre-built session-store client. Takes precedence
	// over Querier when both are set. Preferred wiring from runtime so
	// every subsystem shares the same underlying client.
	Sessions *sessstore.Client

	// AgentID / AgentShort are forwarded to the internal sessstore
	// client when the Manager builds it from Querier. Ignored when
	// Sessions is provided.
	AgentID    string
	AgentShort string

	// Constitution is the base system prompt text.
	Constitution string

	// InlineBuilder is called by LoadSkill when a skill declares an
	// inline MCP endpoint (not a named Provider reference). It builds
	// a tools.Provider under the given synthetic name, typically by
	// delegating to providers.Build with type=mcp. May be nil — skills
	// that only use named providers work without it.
	InlineBuilder InlineProviderFactory

	// Classifier persists conversation events (user_message / llm_response
	// / tool_call / tool_result) asynchronously via hub.AppendEvent.
	// When nil, IngestADKEvent is a no-op on the persistence side. The
	// classifier goroutine is expected to be running when set; it is not
	// started by the manager.
	Classifier *Classifier

	// Scheduler queues post-session reviews on Delete. When nil, reviews
	// are never queued — useful for tests that drive the reviewer directly.
	Scheduler ReviewQueuer

	// Logger may be nil; defaults to slog.Default.
	Logger *slog.Logger
}

// ReviewQueuer abstracts pkg/scheduler.Scheduler so the session
// package does not import the scheduler (which would create a cycle
// once scheduler depends on learning → session state). One method:
// QueueReview(sessionID string).
type ReviewQueuer interface {
	QueueReview(sessionID string)
}

// InlineProviderAuth bundles the auth-related fields from a skill's
// inline provider spec. Lets InlineProviderFactory pick the right
// transport wrapping (Bearer / static header / none) without pulling
// skills types into the factory contract.
type InlineProviderAuth struct {
	Type        string // "hugr" | "header" | "auto" | ""
	Name        string // cfg.Auth[Name] when Type == "hugr"
	HeaderName  string // Type == "header"
	HeaderValue string // Type == "header"
}

// InlineProviderFactory builds an anonymous MCP provider for a skill's
// inline endpoint spec. Called at most once per distinct (skillName,
// providerName) combination — the resulting provider is registered in
// tools.Manager under the provided synthetic name.
type InlineProviderFactory func(name, endpoint string, auth InlineProviderAuth, logger *slog.Logger) (tools.Provider, error)

// Manager owns runtime sessions. Implements adksession.Service and
// *Manager. System tools no longer live here — they
// come from a tools.Provider declared in config.yaml (`type: system`,
// `suite: skills`).
type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*Session // sessionID → session

	skills        skills.Manager
	tools         *tools.Manager
	hub           *sessstore.Client
	constitution  string
	logger        *slog.Logger
	inlineBuilder InlineProviderFactory
	classifier    *Classifier
	scheduler     ReviewQueuer
}

var (
	_ adksession.Service = (*Manager)(nil)
	_ *Manager           = (*Manager)(nil)
)

// New builds a Manager. When cfg.Querier is non-nil, the manager builds
// its own sessstore client internally for hub persistence; otherwise it
// runs in memory-only mode (useful for tests).
func New(cfg Config) (*Manager, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	hub := cfg.Sessions
	if hub == nil && cfg.Querier != nil {
		c, err := sessstore.New(cfg.Querier, sessstore.Options{
			AgentID: cfg.AgentID, AgentShort: cfg.AgentShort, Logger: cfg.Logger,
		})
		if err != nil {
			return nil, fmt.Errorf("session: build sessions store: %w", err)
		}
		hub = c
	}
	return &Manager{
		sessions:      make(map[string]*Session),
		skills:        cfg.Skills,
		tools:         cfg.Tools,
		hub:           hub,
		constitution:  cfg.Constitution,
		logger:        cfg.Logger,
		inlineBuilder: cfg.InlineBuilder,
		classifier:    cfg.Classifier,
		scheduler:     cfg.Scheduler,
	}, nil
}

// publishEvent routes an ADK event to the classifier when one is
// attached. Called from Session.IngestADKEvent on every turn — must
// never block on I/O.
func (m *Manager) publishEvent(sessionID string, ev *adksession.Event) {
	if m.classifier == nil || ev == nil {
		return
	}
	m.classifier.Publish(Envelope{SessionID: sessionID, Event: ev})
}

// ------------------------------------------------------------
// *Manager
// ------------------------------------------------------------

// Session returns the runtime session matching id, or an error.
func (m *Manager) Session(id string) (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session: %q not tracked", id)
	}
	return s, nil
}

// SkillsAccessor wraps the Manager to satisfy skills.SessionAccessor.
// Go method-return covariance is strict — Manager.Session(id) returns
// *Session (concrete) rather than skills.Session (interface), so a
// tiny adapter is the cleanest bridge. Lives in pkg/sessions because
// the adapter is session-domain glue, not a cmd-level concern.
func (m *Manager) SkillsAccessor() skills.SessionAccessor {
	return skillsAccessor{m: m}
}

type skillsAccessor struct{ m *Manager }

func (a skillsAccessor) Session(id string) (skills.Session, error) {
	return a.m.Session(id)
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

// RestoreOpen creates lightweight stubs for every active session row
// in hub.db. No events replayed and no skills bound yet — that work
// is deferred to Session.ensureMaterialized on first Get, so startup
// stays cheap regardless of history size.
//
// app_name for restored sessions is read from sessions.metadata
// (written by Create). Empty app_name is allowed — older rows may
// predate this.
func (m *Manager) RestoreOpen(ctx context.Context) error {
	if m.hub == nil {
		return nil
	}
	rows, err := m.hub.ListActiveSessions(ctx)
	if err != nil {
		return fmt.Errorf("session: list active: %w", err)
	}
	for _, row := range rows {
		app := ""
		if row.Metadata != nil {
			if v, ok := row.Metadata["app_name"].(string); ok {
				app = v
			}
		}
		sess := m.newLocal(row.ID, app, row.OwnerID)
		m.mu.Lock()
		m.sessions[row.ID] = sess
		m.mu.Unlock()
	}
	m.logger.Info("session: stubs restored", "count", len(rows))
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
	sess.markMaterialized() // fresh session — nothing to replay from hub
	m.sessions[id] = sess
	m.mu.Unlock()

	if req.State != nil {
		for k, v := range req.State {
			_ = sess.state.Set(k, v)
		}
	}

	if m.hub != nil {
		meta := map[string]any{}
		if req.AppName != "" {
			meta["app_name"] = req.AppName
		}
		row := sessstore.Record{
			ID:       id,
			AgentID:  m.hub.AgentID(),
			OwnerID:  req.UserID,
			Status:   "active",
			Metadata: meta,
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
	if err := sess.ensureMaterialized(ctx); err != nil {
		return nil, fmt.Errorf("session %q materialize: %w", req.SessionID, err)
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
	sess, ok := m.sessions[req.SessionID]
	delete(m.sessions, req.SessionID)
	m.mu.Unlock()

	if ok && sess != nil {
		sess.dropAllBindings(ctx)
	}

	if m.hub != nil {
		if err := m.hub.UpdateSessionStatus(ctx, req.SessionID, "completed"); err != nil {
			m.logger.Warn("session: hub.UpdateSessionStatus", "id", req.SessionID, "err", err)
		}
	}
	if m.scheduler != nil {
		m.scheduler.QueueReview(req.SessionID)
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
