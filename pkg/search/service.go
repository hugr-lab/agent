package search

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/adk/tool"

	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
	"github.com/hugr-lab/query-engine/types"
)

// ServiceName is the provider name *Service registers under in
// tools.Manager. Skills reference it via
// `providers: [{provider: _search}]`.
const ServiceName = "_search"

// sessionReader is the slim interface the search tools need from
// pkg/sessions/store. Satisfied by *sessstore.Client in production.
// Lets tests substitute a fake.
type sessionReader interface {
	GetSession(ctx context.Context, id string) (*sessstore.Record, error)
}

// Deps bundles the search service's external dependencies.
type Deps struct {
	// Querier is the hub.db GraphQL surface. Required.
	Querier types.Querier

	// SessionReader resolves session_type for scope authorization +
	// owner_id for scope=user corpus expansion. Required.
	SessionReader sessionReader

	// AgentID scopes every query. Required.
	AgentID string

	// Logger defaults to slog.Default when nil.
	Logger *slog.Logger

	// EmbedderEnabled gates the `semantic:` GraphQL argument. When
	// false, the tool falls back to substring matching.
	EmbedderEnabled bool

	// Now overrides time.Now for tests; defaults to time.Now.UTC.
	Now func() time.Time

	// HalfLifeMission / HalfLifeSession / HalfLifeUser — defaults
	// applied when the caller omits `half_life`. Zero falls back
	// to documented defaults (1h / 24h / 168h).
	HalfLifeMission time.Duration
	HalfLifeSession time.Duration
	HalfLifeUser    time.Duration

	// DefaultLimit caps the result count when caller omits `last_n`.
	// Zero falls back to 20.
	DefaultLimit int

	// UserBatchAliasLimit — when scope=user has more roots than this,
	// the tool falls back to sequential per-root queries instead of
	// alias-multiplexed batched ones. Zero falls back to 50.
	UserBatchAliasLimit int
}

// Service is the multi-horizon session-context search subsystem.
// Implements tools.Provider — same Manager-pattern shipped by
// pkg/memory and pkg/artifacts.
type Service struct {
	q       types.Querier
	sessR   sessionReader
	agentID string
	logger  *slog.Logger

	embedderEnabled bool

	halfLifeMission time.Duration
	halfLifeSession time.Duration
	halfLifeUser    time.Duration

	defaultLimit       int
	userBatchAliasMax  int

	nowFn func() time.Time

	tools []tool.Tool
}

// New constructs the Service. Returns error when required deps are
// missing.
func New(deps Deps) (*Service, error) {
	if deps.Querier == nil {
		return nil, fmt.Errorf("search: New requires Querier")
	}
	if deps.SessionReader == nil {
		return nil, fmt.Errorf("search: New requires SessionReader")
	}
	if deps.AgentID == "" {
		return nil, fmt.Errorf("search: New requires AgentID")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	if deps.HalfLifeMission == 0 {
		deps.HalfLifeMission = 1 * time.Hour
	}
	if deps.HalfLifeSession == 0 {
		deps.HalfLifeSession = 24 * time.Hour
	}
	if deps.HalfLifeUser == 0 {
		deps.HalfLifeUser = 168 * time.Hour
	}
	if deps.DefaultLimit <= 0 {
		deps.DefaultLimit = 20
	}
	if deps.UserBatchAliasLimit <= 0 {
		deps.UserBatchAliasLimit = 50
	}

	s := &Service{
		q:                  deps.Querier,
		sessR:              deps.SessionReader,
		agentID:            deps.AgentID,
		logger:             deps.Logger,
		embedderEnabled:    deps.EmbedderEnabled,
		halfLifeMission:    deps.HalfLifeMission,
		halfLifeSession:    deps.HalfLifeSession,
		halfLifeUser:       deps.HalfLifeUser,
		defaultLimit:       deps.DefaultLimit,
		userBatchAliasMax:  deps.UserBatchAliasLimit,
		nowFn:              deps.Now,
	}
	s.tools = []tool.Tool{
		&sessionContextTool{s: s},
		&sessionEventsTool{s: s},
	}
	return s, nil
}

// Name implements tools.Provider.
func (s *Service) Name() string { return ServiceName }

// Tools implements tools.Provider.
func (s *Service) Tools() []tool.Tool { return s.tools }

// AgentID returns the configured agent scope.
func (s *Service) AgentID() string { return s.agentID }

// halfLifeFor returns the operator-tuned default half-life for a
// scope. Caller can override via the tool's `half_life` argument.
func (s *Service) halfLifeFor(scope Scope) time.Duration {
	switch scope {
	case ScopeMission:
		return s.halfLifeMission
	case ScopeSession:
		return s.halfLifeSession
	case ScopeUser:
		return s.halfLifeUser
	default:
		return s.halfLifeMission
	}
}
