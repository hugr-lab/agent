// Package runtime assembles the full hugr-agent runtime (local engine,
// session manager, memory/chat subsystems, tool providers, root LLM
// agent, background workers) from a resolved *config.Config and a
// minimal set of externally-provided auth / remote-client hooks.
//
// This is the single source of truth for "how the agent is wired"
// and is shared by:
//   - cmd/agent/runtime.go — production bootstrap (supplies auth +
//     hugr client through Options and wraps the returned *Runtime in
//     its own listener-management shell)
//   - tests/scenarios/harness — integration scenario runner (supplies
//     a zero-valued Options so everything funnels through the local
//     engine; no external auth / remote client needed)
package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/tool"

	hugen "github.com/hugr-lab/hugen/pkg/agent"
	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/chatcontext"
	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/memory"
	learnstore "github.com/hugr-lab/hugen/pkg/memory/learning/store"
	memstore "github.com/hugr-lab/hugen/pkg/memory/store"
	"github.com/hugr-lab/hugen/pkg/missions"
	missionsexec "github.com/hugr-lab/hugen/pkg/missions/executor"
	missionsfollowup "github.com/hugr-lab/hugen/pkg/missions/followup"
	missionsplan "github.com/hugr-lab/hugen/pkg/missions/planner"
	missionsstore "github.com/hugr-lab/hugen/pkg/missions/store"
	"github.com/hugr-lab/hugen/pkg/models"
	"github.com/hugr-lab/hugen/pkg/scheduler"
	"github.com/hugr-lab/hugen/pkg/sessions"
	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
	"github.com/hugr-lab/hugen/pkg/skills"
	"github.com/hugr-lab/hugen/pkg/store/local"
	"github.com/hugr-lab/hugen/pkg/tools"

	qe "github.com/hugr-lab/query-engine"
	qetypes "github.com/hugr-lab/query-engine/types"
)

// Runtime is the assembled agent runtime. Caller owns Close().
//
// Engine is the embedded hugr query-engine (non-nil when
// cfg.LocalDBEnabled). Querier is the effective Querier used for
// memory / sessions / tools — equals Engine in local mode, equals
// the external remote client in hub mode.
type Runtime struct {
	Agent      agent.Agent
	Sessions   *sessions.Manager
	Skills     skills.Manager
	Tools      *tools.Manager
	Classifier *sessions.Classifier
	Scheduler  *scheduler.Scheduler

	// Missions is the phase-2 mission graph executor. Nil when no
	// missions-config was provided at runtime.Build.
	Missions *missionsexec.Executor

	// Engine is the embedded query-engine (nil in hub-only mode).
	Engine *qe.Service
	// Querier is the active Querier the runtime wires memory +
	// sessions + tools to (Engine or the caller-provided HugrClient).
	Querier qetypes.Querier

	bgCtx      context.Context
	bgCancel   context.CancelFunc
	extraClose []func()
	logger     *slog.Logger

	closeOnce sync.Once
}

// Options carries the externally-built pieces the runtime cannot
// construct itself without pulling in auth/bootstrap dependencies.
// Zero values produce a fully-local runtime (scenarios, dev tests).
type Options struct {
	// AuthStores feeds the inline MCP builder + any external MCP
	// providers declared in cfg.Providers. Nil = no auth wrapping.
	AuthStores map[string]auth.TokenStore

	// HugrClient is the remote GraphQL querier. When nil, the local
	// engine is used as both the "local" and "remote" slot in the
	// router + as the memory/sessions querier. Production wiring
	// supplies the live *client.Client here.
	HugrClient qetypes.Querier

	// HugrClientClose is called from Runtime.Close when set; lets
	// production hand over subscription cleanup without this package
	// importing hugr client types directly.
	HugrClientClose func()

	// BaseTransport for outbound MCP connections; defaults to
	// http.DefaultTransport.
	BaseTransport http.RoundTripper
}

// Build wires the full runtime. Returned Runtime owns no external
// dependencies unless HugrClientClose was supplied — Close()
// coordinates shutdown of the bg workers + engine + any caller-owned
// resources via AppendCloser / HugrClientClose.
func Build(
	ctx context.Context,
	cfg *config.Config,
	logger *slog.Logger,
	opts Options,
) (*Runtime, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if opts.BaseTransport == nil {
		opts.BaseTransport = http.DefaultTransport
	}

	constitution, err := readConstitution(cfg)
	if err != nil {
		return nil, err
	}

	skillsPath := cfg.Skills.Path
	if skillsPath == "" {
		skillsPath = "./skills"
	}
	skillsMgr, err := skills.NewFileManager(skillsPath)
	if err != nil {
		return nil, fmt.Errorf("runtime: skills: %w", err)
	}
	toolsMgr := tools.New(logger)
	tokens := models.NewTokenEstimator()

	rt := &Runtime{
		Skills: skillsMgr,
		Tools:  toolsMgr,
		logger: logger,
	}
	closeOnErr := func() { rt.Close() }

	var (
		localQuerier qetypes.Querier
		// remoteQuerier falls back to the local engine below when
		// opts.HugrClient is nil (scenario / fully-local mode).
		remoteQuerier = opts.HugrClient
		localModels   []string
	)
	if cfg.LocalDBEnabled {
		engine, ms, err := buildLocalHugr(ctx, cfg, logger)
		if err != nil {
			if opts.HugrClientClose != nil {
				opts.HugrClientClose()
			}
			return nil, err
		}
		rt.Engine = engine
		localQuerier = engine
		localModels = ms
	} else {
		logger.Info("runtime: hub mode", "url", cfg.Hugr.URL)
	}
	if remoteQuerier == nil {
		// No external remote client — funnel everything through local
		// (scenario / fully-local mode). Router still needs a non-nil
		// remote to satisfy its contract.
		remoteQuerier = localQuerier
	}
	memoryQuerier := remoteQuerier
	if localQuerier != nil {
		memoryQuerier = localQuerier
	}
	rt.Querier = memoryQuerier

	router := models.NewRouter(
		localQuerier,
		remoteQuerier,
		localModels,
		cfg.LLM,
		models.WithLogger(logger),
		models.WithToolChoiceFunc(func() string { return "auto" }),
	).WithLogger(logger)
	for intentName, modelName := range cfg.LLM.Routes {
		logger.Info("runtime: intent route", "intent", intentName, "model", modelName)
	}

	// Hub clients — shared across every subsystem (classifier, sessions
	// manager, memory service, reviewer, compactor, consolidator,
	// verifier, injector). Every hub wired to memoryQuerier.
	embedderEnabled := cfg.Embedding.Dimension > 0 && cfg.Embedding.Model != ""
	sessHub, err := sessstore.New(memoryQuerier, sessstore.Options{
		AgentID: cfg.Identity.ID, AgentShort: cfg.Identity.ShortID, Logger: logger,
		EmbedderEnabled: embedderEnabled,
	})
	if err != nil {
		closeOnErr()
		return nil, fmt.Errorf("runtime: build sessions store: %w", err)
	}
	memHub, err := memstore.New(memoryQuerier, memstore.Options{
		AgentID: cfg.Identity.ID, AgentShort: cfg.Identity.ShortID, Logger: logger,
		EmbedderEnabled: embedderEnabled,
	})
	if err != nil {
		closeOnErr()
		return nil, fmt.Errorf("runtime: build memory store: %w", err)
	}
	learnHub, err := learnstore.New(memoryQuerier, learnstore.Options{
		AgentID: cfg.Identity.ID, AgentShort: cfg.Identity.ShortID, Logger: logger,
	})
	if err != nil {
		closeOnErr()
		return nil, fmt.Errorf("runtime: build learning store: %w", err)
	}

	cls := sessions.NewClassifierWithHub(sessHub, logger, sessions.DefaultClassifierBuffer)

	loadSkillMemory := func(ctx context.Context, name string) (*skills.SkillMemoryConfig, error) {
		sk, err := skillsMgr.Load(ctx, name)
		if err != nil || sk == nil {
			return nil, err
		}
		return sk.Memory, nil
	}

	mem, err := memory.New(memory.Options{
		Querier:         memoryQuerier,
		AgentID:         cfg.Identity.ID,
		AgentShort:      cfg.Identity.ShortID,
		Logger:          logger,
		Memory:          memHub,
		Learning:        learnHub,
		Sessions:        sessHub,
		Router:          router,
		Tokens:          tokens,
		Config:          cfg.Memory,
		LoadSkillMemory: loadSkillMemory,
	})
	if err != nil {
		closeOnErr()
		return nil, fmt.Errorf("runtime: build memory: %w", err)
	}

	chat, err := chatcontext.New(chatcontext.Options{
		Querier:         memoryQuerier,
		AgentID:         cfg.Identity.ID,
		AgentShort:      cfg.Identity.ShortID,
		Logger:          logger,
		Memory:          memHub,
		Sessions:        sessHub,
		Router:          router,
		Tokens:          tokens,
		Intent:          models.IntentDefault,
		Threshold:       cfg.ChatContext.CompactionThreshold,
		LoadSkillMemory: loadSkillMemory,
	})
	if err != nil {
		closeOnErr()
		return nil, fmt.Errorf("runtime: build chatcontext: %w", err)
	}

	sched := scheduler.New(logger)
	if err := mem.RegisterTasks(sched); err != nil {
		closeOnErr()
		return nil, fmt.Errorf("runtime: register memory tasks: %w", err)
	}
	rt.Classifier = cls
	rt.Scheduler = sched

	// Circular dep resolved via a captured pointer: sessions.Manager
	// needs a SubAgentToolBuilder at Config time (per-skill subagent_*
	// tools bind during LoadSkill, including autoload-at-Create), and
	// Dispatcher needs *sessions.Manager. Back-fill dispatcherRef
	// after NewDispatcher below — until then the closure returns nil
	// and Session.bindSubAgents skips (safe: the generic
	// `subagent_dispatch` tool from `_system` still covers dispatch).
	var dispatcherRef *hugen.Dispatcher
	subagentToolBuilder := func(skillName, role string, spec skills.SubAgentSpec) tool.Tool {
		if dispatcherRef == nil {
			return nil
		}
		return dispatcherRef.ToolFor(skillName, role, spec)
	}

	sessionMgr, err := sessions.New(sessions.Config{
		Skills:              skillsMgr,
		Tools:               toolsMgr,
		Sessions:            sessHub,
		AgentID:             cfg.Identity.ID,
		AgentShort:          cfg.Identity.ShortID,
		Constitution:        constitution,
		Logger:              logger,
		Classifier:          cls,
		Scheduler:           mem.Reviewer(),
		SubAgentToolBuilder: subagentToolBuilder,
		InlineBuilder: func(name, endpoint string, a sessions.InlineProviderAuth, lg *slog.Logger) (tools.Provider, error) {
			return tools.NewMCPProvider(tools.MCPSpec{
				Name:            name,
				Endpoint:        endpoint,
				Auth:            a.Name,
				AuthType:        a.Type,
				AuthHeaderName:  a.HeaderName,
				AuthHeaderValue: a.HeaderValue,
				AuthStores:      opts.AuthStores,
				BaseTransport:   opts.BaseTransport,
				Config:          cfg.MCP,
				Logger:          lg,
			})
		},
	})
	if err != nil {
		closeOnErr()
		return nil, fmt.Errorf("runtime: build session manager: %w", err)
	}
	rt.Sessions = sessionMgr

	cls.AttachManager(sessionMgr)
	if err := chat.AttachSessions(sessionMgr); err != nil {
		closeOnErr()
		return nil, fmt.Errorf("runtime: attach chatcontext service: %w", err)
	}

	memService, err := memory.NewService(memoryQuerier, sessionMgr, memory.ServiceOptions{
		AgentID: cfg.Identity.ID, AgentShort: cfg.Identity.ShortID, Logger: logger,
		Memory: memHub, Sessions: sessHub,
	})
	if err != nil {
		closeOnErr()
		return nil, fmt.Errorf("runtime: build memory service: %w", err)
	}
	toolsMgr.AddProvider(skills.NewService(sessionMgr.SkillsAccessor()))
	toolsMgr.AddProvider(memService)
	toolsMgr.AddProvider(chat.Provider())

	dispatcher, err := hugen.NewDispatcher(hugen.DispatcherConfig{
		Sessions: sessionMgr,
		Skills:   skillsMgr,
		Router:   router,
		Logger:   logger,
	})
	if err != nil {
		closeOnErr()
		return nil, fmt.Errorf("runtime: build sub-agent dispatcher: %w", err)
	}
	// Back-fill the closure captured by sessions.Config.SubAgentToolBuilder
	// now that dispatcher exists. Any skill loaded from this point on
	// (autoload fires lazily on first Create) gets subagent_<skill>_<role>
	// tools wired straight through Dispatcher.ToolFor.
	dispatcherRef = dispatcher
	subagentSvc, err := hugen.NewSubAgentService(dispatcher, sessionMgr, skillsMgr)
	if err != nil {
		closeOnErr()
		return nil, fmt.Errorf("runtime: build sub-agent service: %w", err)
	}
	toolsMgr.AddProvider(subagentSvc)

	// Spec 007 — mission graph runtime. Planner + Store + Executor
	// share the same sessstore client; the driver adapts Dispatcher
	// to the Executor's MissionDriver surface. The tools.Provider
	// (missions.Service) is registered in toolsMgr under the
	// ServiceName key, and skills that want mission_plan /
	// mission_status declare `provider: _mission_tools` in their
	// frontmatter — same pattern as _memory / _context / _system.
	missionsStore := missionsstore.New(sessHub, memoryQuerier, logger)
	plannerHeader, err := loadCoordinatorPrompt(skillsPath, "planner-prompt.md", logger)
	if err != nil {
		closeOnErr()
		return nil, fmt.Errorf("runtime: load planner prompt: %w", err)
	}
	missionsPlanner := missionsplan.New(router, logger, missionsplan.Options{
		PromptHeader: plannerHeader,
	})
	missionsDriver := &dispatcherMissionDriver{
		dispatcher: dispatcher,
		skills:     skillsMgr,
		logger:     logger,
	}
	missionsExec := missionsexec.New(missionsexec.Config{
		Store:       missionsStore,
		Events:      sessHub,
		Driver:      missionsDriver,
		Logger:      logger,
		Parallelism: 4,
		StaleAfter:  cfg.Missions.StaleMissionTimeout,
	})
	// Spec 007 US5: rebuild every coordinator's DAG from hub.db
	// before the scheduler kicks in. Stale-active rows get marked
	// abandoned with reason="restart: stale"; fresh rows are
	// reattached without re-emitting mission_spawn (FR-020).
	if _, err := missionsExec.RestoreState(ctx); err != nil {
		closeOnErr()
		return nil, fmt.Errorf("runtime: missions restore: %w", err)
	}
	missionsSvc := missions.NewService(missions.Config{
		Planner:  missionsPlanner,
		Executor: missionsExec,
		Sessions: sessionMgr,
		Skills:   skillsMgr,
		Events:   sessHub,
	})
	toolsMgr.AddProvider(missionsSvc)
	// Spec 007 US6 — sub-agent spawn surface. Lives in its own
	// provider (`_mission_spawn`) so the coordinator's _coordinator
	// skill can opt in to mission_tools without inadvertently
	// exposing spawn_sub_mission. The skills/_subagent autoload
	// skill (autoload_for: [subagent]) wires the provider onto every
	// sub-agent session; the tool itself enforces can_spawn +
	// max_depth at run time.
	spawnSvc := missions.NewSpawnService(missions.SpawnConfig{
		Executor:      missionsExec,
		Sessions:      sessionMgr,
		Skills:        skillsMgr,
		MaxSpawnDepth: cfg.Missions.MaxSpawnDepthAgent,
	})
	toolsMgr.AddProvider(spawnSvc)
	rt.Missions = missionsExec
	// Drop cached plans when the coordinator session closes so a
	// restarted conversation never serves stale DAG ids.
	sessionMgr.OnSessionClose(missionsPlanner.OnCoordinatorClose)
	// Cancel + abandon every still-running mission when its coordinator
	// closes — FR-011. Hook receives no ctx, so the cascade runs on a
	// fresh background ctx; never blocks the close goroutine for long
	// since Cancel only walks the in-memory DAG + writes terminal rows.
	sessionMgr.OnSessionClose(func(coordID string) {
		missionsExec.AbandonCoordinator(context.Background(), coordID)
	})

	// Drive the scheduler every 2s — the Executor reconciles its
	// in-memory DAGs + promotes ready missions to running + drains
	// terminal goroutines on each tick. TryLock guards overlap.
	if err := sched.Every("missions-tick", 2*time.Second, func(ctx context.Context) error {
		missionsExec.Tick(ctx)
		return nil
	}); err != nil {
		closeOnErr()
		return nil, fmt.Errorf("runtime: register missions-tick: %w", err)
	}

	logger.Info("runtime: internal services registered",
		"providers", []string{
			skills.ServiceName, memory.ServiceName, chatcontext.ServiceName,
			hugen.SubAgentProviderName, missions.ServiceName,
		})

	for _, pc := range cfg.Providers {
		if pc.Type == "system" {
			continue
		}
		if pc.Type != "mcp" {
			closeOnErr()
			return nil, fmt.Errorf("runtime: provider %q: only type=mcp is supported", pc.Name)
		}
		p, err := tools.NewMCPProvider(tools.MCPSpec{
			Name:            pc.Name,
			Endpoint:        pc.Endpoint,
			Transport:       pc.Transport,
			Command:         pc.Command,
			Args:            pc.Args,
			Env:             pc.Env,
			Auth:            pc.Auth,
			AuthType:        pc.AuthType,
			AuthHeaderName:  pc.AuthHeaderName,
			AuthHeaderValue: pc.AuthHeaderValue,
			AuthStores:      opts.AuthStores,
			BaseTransport:   opts.BaseTransport,
			Config:          cfg.MCP,
			Logger:          logger,
		})
		if err != nil {
			closeOnErr()
			return nil, fmt.Errorf("runtime: provider %q: %w", pc.Name, err)
		}
		toolsMgr.AddProvider(p)
		logger.Info("runtime: provider registered", "name", pc.Name, "type", pc.Type)
	}

	instruction := mem.InstructionProvider(hugen.BaseInstructionProvider(sessionMgr))

	// Spec 007 follow-up router — slots before the compactor so a
	// short-circuited turn skips both the model call AND the
	// compactor's work. Enabled by default; operators flip off via
	// cfg.Missions.FollowUpEnabled when the behaviour needs tuning.
	// Classification rules live in skills/_coordinator/followup-
	// classifier.md so operators edit prompt prose without rebuilding.
	followupHeader, err := loadCoordinatorPrompt(skillsPath, "followup-classifier.md", logger)
	if err != nil {
		closeOnErr()
		return nil, fmt.Errorf("runtime: load followup classifier prompt: %w", err)
	}
	followupRouter := missionsfollowup.New(missionsfollowup.Config{
		Executor:     missionsExec,
		Router:       router,
		Logger:       logger,
		Threshold:    cfg.Missions.FollowUpSimilarityThreshold,
		TieBand:      cfg.Missions.FollowUpTieBand,
		Timeout:      cfg.Missions.ClassifierTimeout,
		Enabled:      cfg.Missions.FollowUpEnabled,
		PromptHeader: followupHeader,
	})

	a, err := hugen.NewAgent(hugen.Runtime{
		Router:   router,
		Sessions: sessionMgr,
		Tokens:   tokens,
		ExtraBeforeCallbacks: []llmagent.BeforeModelCallback{
			followupRouter.Callback(),
			chat.Callback(),
		},
		InstructionProvider: instruction,
		Logger:              logger,
	})
	if err != nil {
		closeOnErr()
		return nil, fmt.Errorf("runtime: create agent: %w", err)
	}
	rt.Agent = a

	if err := sessionMgr.RestoreOpen(ctx); err != nil {
		logger.Warn("runtime: restore open sessions", "err", err)
	}

	bgCtx, bgCancel := context.WithCancel(ctx)
	rt.bgCtx = bgCtx
	rt.bgCancel = bgCancel

	go cls.Run(bgCtx)
	sched.Start(bgCtx)
	hugen.StartSessionCleanup(bgCtx, sessionMgr, 1*time.Hour, logger)

	if opts.HugrClientClose != nil {
		rt.extraClose = append(rt.extraClose, opts.HugrClientClose)
	}
	return rt, nil
}

// AppendCloser registers an extra cleanup callback run during Close.
// Idempotent w.r.t. repeat calls with the same function.
func (r *Runtime) AppendCloser(fn func()) {
	if r == nil || fn == nil {
		return
	}
	r.extraClose = append(r.extraClose, fn)
}

// Close shuts down background workers + engine + any caller-supplied
// closers. Safe to call multiple times.
func (r *Runtime) Close() {
	if r == nil {
		return
	}
	r.closeOnce.Do(func() {
		if r.bgCancel != nil {
			r.bgCancel()
		}
		const budget = 5 * time.Second
		deadline := time.Now().Add(budget)
		var wg sync.WaitGroup
		if r.Scheduler != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				select {
				case <-r.Scheduler.Done():
				case <-time.After(time.Until(deadline)):
					if r.logger != nil {
						r.logger.Warn("runtime: scheduler did not stop within budget")
					}
				}
			}()
		}
		if r.Classifier != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				drainCtx, cancel := context.WithTimeout(context.Background(), time.Until(deadline))
				defer cancel()
				if err := r.Classifier.Drain(drainCtx, time.Until(deadline)); err != nil && r.logger != nil {
					r.logger.Warn("runtime: classifier drain", "err", err)
				}
			}()
		}
		wg.Wait()
		if r.Engine != nil {
			if err := r.Engine.Close(); err != nil && r.logger != nil {
				r.logger.Error("runtime: engine close", "err", err)
			}
		}
		for _, fn := range r.extraClose {
			fn()
		}
	})
}

// readConstitution loads the agent constitution file from the path
// declared in cfg.Agent.Constitution. Missing / unreadable file is a
// fatal config error since the constitution drives the system prompt.
func readConstitution(cfg *config.Config) (string, error) {
	path := cfg.Agent.Constitution
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("runtime: read constitution %s: %w", path, err)
	}
	return string(data), nil
}

// loadCoordinatorPrompt reads an operator-editable prompt prose file
// from skills/_coordinator/<name>. Missing file is fine — callers
// fall back to their embedded defaults. Read errors other than
// not-found are surfaced so a corrupted file is noticed at boot
// rather than masked.
func loadCoordinatorPrompt(skillsPath, name string, logger *slog.Logger) (string, error) {
	if skillsPath == "" {
		return "", nil
	}
	path := filepath.Join(skillsPath, "_coordinator", name)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			logger.Info("runtime: coordinator prompt not found, using embedded default",
				"path", path)
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return string(data), nil
}

// buildLocalHugr brings up the embedded query-engine backed by
// cfg.LocalDB and returns it along with the LLM model names the router
// should route to it.
func buildLocalHugr(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*qe.Service, []string, error) {
	engine, err := local.New(ctx, cfg.LocalDB, cfg.Identity, cfg.Embedding, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("runtime: local engine: %w", err)
	}
	ms := make([]string, 0, len(cfg.LocalDB.Models))
	for _, m := range cfg.LocalDB.Models {
		if m.Type == "embedding" {
			continue
		}
		ms = append(ms, m.Name)
	}
	return engine, ms, nil
}
