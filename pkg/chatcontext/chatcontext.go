// Package chatcontext owns the context-window subsystem: the
// _context tools.Provider (context_status, context_intro) plus the
// Compactor BeforeModelCallback that folds oldest turn groups into
// summary messages when the request approaches the token budget.
//
// External consumers should prefer the ChatContext facade below
// (New + methods) over wiring Compactor / Service individually.
package chatcontext

import (
	"context"
	"fmt"
	"log/slog"

	memstore "github.com/hugr-lab/hugen/pkg/memory/store"
	"github.com/hugr-lab/hugen/pkg/models"
	"github.com/hugr-lab/hugen/pkg/sessions"
	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
	"github.com/hugr-lab/hugen/pkg/skills"
	"github.com/hugr-lab/query-engine/types"
	"google.golang.org/adk/agent/llmagent"
)

// ChatContext is the context-window subsystem packaged as a single
// facade. It owns the Compactor (BeforeModelCallback) + the Service
// (tools.Provider). Service construction is deferred to
// AttachSessions since it needs a live *sessions.Manager.
type ChatContext struct {
	compactor *Compactor
	service   *Service

	// deferred Service deps.
	querier   types.Querier
	memHub    *memstore.Client
	sessHub   *sessstore.Client
	agentID   string
	agentShrt string
	logger    *slog.Logger
}

// Options bundles everything New needs for the compactor. Service
// wiring happens later via AttachSessions because it requires the
// SessionManager, which is built after ChatContext (the session
// manager's InlineBuilder + classifier + scheduler are all
// independent of chatcontext — but the service needs the manager).
type Options struct {
	Querier    types.Querier
	AgentID    string
	AgentShort string
	Logger     *slog.Logger

	// Pre-built hub clients (preferred).
	Memory   *memstore.Client
	Sessions *sessstore.Client

	Router *models.Router
	Tokens *models.TokenEstimator

	// Intent picks which model's context budget the compactor's
	// trigger threshold tracks (spec 006 §1). Empty defaults to
	// models.IntentDefault — the coordinator's strong-model budget,
	// which preserves the pre-006 behaviour. Sub-agent dispatch
	// constructs its own per-mission compactor with the role's
	// intent so cheap-model specialists compact at the cheap-model
	// window.
	Intent models.Intent

	Threshold float64
	MinTurns  int

	LoadSkillMemory func(ctx context.Context, skillName string) (*skills.SkillMemoryConfig, error)
}

// New builds the Compactor. Service construction waits for
// AttachSessions.
func New(opts Options) (*ChatContext, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	compactor, err := NewCompactor(CompactorOptions{
		Querier:         opts.Querier,
		AgentID:         opts.AgentID,
		AgentShort:      opts.AgentShort,
		Router:          opts.Router,
		Tokens:          opts.Tokens,
		Intent:          opts.Intent,
		Threshold:       opts.Threshold,
		MinTurns:        opts.MinTurns,
		Logger:          opts.Logger,
		Memory:          opts.Memory,
		Sessions:        opts.Sessions,
		LoadSkillMemory: opts.LoadSkillMemory,
	})
	if err != nil {
		return nil, fmt.Errorf("chatcontext: build compactor: %w", err)
	}
	return &ChatContext{
		compactor: compactor,
		querier:   opts.Querier,
		memHub:    opts.Memory,
		sessHub:   opts.Sessions,
		agentID:   opts.AgentID,
		agentShrt: opts.AgentShort,
		logger:    opts.Logger,
	}, nil
}

// AttachSessions builds the Service against the given SessionManager
// and wires the compactor's transcript writer to the same manager so
// compaction events take the per-session write lock (spec 006 seq-
// race fix). Must be called before Provider() is used.
func (c *ChatContext) AttachSessions(sm *sessions.Manager) error {
	svc, err := NewService(c.querier, sm, ServiceOptions{
		AgentID: c.agentID, AgentShort: c.agentShrt, Logger: c.logger,
		Memory: c.memHub, Sessions: c.sessHub,
	})
	if err != nil {
		return fmt.Errorf("chatcontext: build service: %w", err)
	}
	c.service = svc
	c.compactor.AttachWriter(sm)
	return nil
}

// Provider returns the _context tools provider for tools.Manager.
// Panics if AttachSessions hasn't been called — caller bug.
func (c *ChatContext) Provider() *Service {
	if c.service == nil {
		panic("chatcontext: Provider called before AttachSessions")
	}
	return c.service
}

// Callback returns the BeforeModelCallback that triggers compaction
// when the request crosses the token-budget threshold.
func (c *ChatContext) Callback() llmagent.BeforeModelCallback {
	return c.compactor.Callback()
}
