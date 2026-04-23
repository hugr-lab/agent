// Package memory owns the long-term memory subsystem: tools exposed
// to the LLM (memory_* via tools.Provider), background review /
// verify / consolidate workers, and the "## Memory Status"
// instruction injector.
//
// External consumers should prefer the Memory facade below (New +
// methods) over wiring Reviewer / Verifier / Consolidator / Injector
// individually. NewService (the tools.Provider) stays standalone
// because it's built after the SessionManager is ready.
package memory

import (
	"context"
	"fmt"
	"log/slog"

	learnstore "github.com/hugr-lab/hugen/pkg/memory/learning/store"
	memstore "github.com/hugr-lab/hugen/pkg/memory/store"
	"github.com/hugr-lab/hugen/pkg/models"
	"github.com/hugr-lab/hugen/pkg/scheduler"
	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
	"github.com/hugr-lab/hugen/pkg/skills"
	"github.com/hugr-lab/query-engine/types"
)

// Memory bundles the background workers + instruction injector that
// the agent runtime normally wires one-by-one. Construct it first
// (before the SessionManager), then pull Reviewer() out for
// sessions.Config.Scheduler, RegisterTasks() on the scheduler, and
// InstructionProvider() to wrap the agent's base provider.
type Memory struct {
	reviewer     *Reviewer
	verifier     *Verifier
	consolidator *Consolidator

	// injector inputs — deferred to InstructionProvider() because
	// the base provider isn't known at New() time.
	querier  types.Querier
	memHub   *memstore.Client
	sessHub  *sessstore.Client
	injOpts  InjectorOptions

	// schedCfg is captured at construction time and applied by
	// RegisterTasks. Kept on the facade so the caller isn't forced
	// to thread SchedulerConfig through two places.
	schedCfg SchedulerConfig
}

// Options bundles everything New needs. Querier is optional when
// pre-built Memory / Learning / Sessions clients are supplied.
type Options struct {
	Querier    types.Querier
	AgentID    string
	AgentShort string
	Logger     *slog.Logger

	// Pre-built hub clients. Runtime should inject these so every
	// subsystem shares a single *Client. When nil, constructors
	// fall back to building from Querier.
	Memory   *memstore.Client
	Learning *learnstore.Client
	Sessions *sessstore.Client

	Router *models.Router
	Tokens *models.TokenEstimator

	Config Config

	// LoadSkillMemory feeds per-skill memory settings into the
	// reviewer. Typically wired to pkg/skills.Manager.Load(...).Memory.
	LoadSkillMemory func(ctx context.Context, skillName string) (*skills.SkillMemoryConfig, error)
}

// New builds the Memory facade. Returns an error when either the
// reviewer, verifier, or consolidator fails to construct (all three
// are required for the full lifecycle).
func New(opts Options) (*Memory, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	reviewer, err := NewReviewer(ReviewerOptions{
		Querier:              opts.Querier,
		AgentID:              opts.AgentID,
		AgentShort:           opts.AgentShort,
		Router:               opts.Router,
		Logger:               opts.Logger,
		Volatility:           opts.Config.VolatilityDuration,
		LoadSkillMemory:      opts.LoadSkillMemory,
		Tokens:               opts.Tokens,
		DefaultWindowTokens:  opts.Config.Review.WindowTokens,
		DefaultOverlapTokens: opts.Config.Review.OverlapTokens,
		DefaultFloorAge:      opts.Config.Review.FloorAge,
		DefaultExcludeTypes:  opts.Config.Review.ExcludeEventTypes,
		Memory:               opts.Memory,
		Learning:             opts.Learning,
		Sessions:             opts.Sessions,
	})
	if err != nil {
		return nil, fmt.Errorf("memory: build reviewer: %w", err)
	}
	verifier, err := NewVerifier(VerifierOptions{
		Querier:    opts.Querier,
		AgentID:    opts.AgentID,
		AgentShort: opts.AgentShort,
		Router:     opts.Router,
		Logger:     opts.Logger,
		Volatility: opts.Config.VolatilityDuration,
		Memory:     opts.Memory,
		Learning:   opts.Learning,
	})
	if err != nil {
		return nil, fmt.Errorf("memory: build verifier: %w", err)
	}
	consolidator, err := NewConsolidator(ConsolidatorOptions{
		Querier:          opts.Querier,
		AgentID:          opts.AgentID,
		AgentShort:       opts.AgentShort,
		HypothesisExpiry: opts.Config.Consolidation.HypothesisExpiry,
		Logger:           opts.Logger,
		Memory:           opts.Memory,
		Learning:         opts.Learning,
	})
	if err != nil {
		return nil, fmt.Errorf("memory: build consolidator: %w", err)
	}
	return &Memory{
		reviewer:     reviewer,
		verifier:     verifier,
		consolidator: consolidator,
		querier:      opts.Querier,
		memHub:       opts.Memory,
		sessHub:      opts.Sessions,
		injOpts: InjectorOptions{
			AgentID: opts.AgentID, AgentShort: opts.AgentShort, Logger: opts.Logger,
			Memory: opts.Memory, Sessions: opts.Sessions,
		},
		schedCfg: opts.Config.Scheduler,
	}, nil
}

// Reviewer returns the Reviewer instance. SessionManager takes it as
// the ReviewQueuer so Delete can queue a post-session review.
func (m *Memory) Reviewer() *Reviewer { return m.reviewer }

// RegisterTasks wires review / verify / consolidate tasks onto sched
// using the facade's Config.Schedule. Idempotent only via scheduler
// semantics — caller should invoke once per Memory.
func (m *Memory) RegisterTasks(sched *scheduler.Scheduler) error {
	return Register(sched, m.reviewer, m.verifier, m.consolidator, m.schedCfg)
}

// InstructionProvider returns an ADK instruction provider that
// prepends a runtime-computed "## Memory Status" block to base's
// output. Cached per-session for 10s.
func (m *Memory) InstructionProvider(base InstructionProvider) InstructionProvider {
	return WrapInstruction(base, m.querier, m.injOpts)
}
