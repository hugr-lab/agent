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
	"google.golang.org/adk/runner"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"

	hugen "github.com/hugr-lab/hugen/pkg/agent"
	"github.com/hugr-lab/hugen/pkg/artifacts"
	artstorage "github.com/hugr-lab/hugen/pkg/artifacts/storage"
	artfs "github.com/hugr-lab/hugen/pkg/artifacts/storage/fs"
	arts3 "github.com/hugr-lab/hugen/pkg/artifacts/storage/s3"
	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/chatcontext"
	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/memory"
	learnstore "github.com/hugr-lab/hugen/pkg/memory/learning/store"
	memstore "github.com/hugr-lab/hugen/pkg/memory/store"
	"github.com/hugr-lab/hugen/pkg/missions"
	missionsexec "github.com/hugr-lab/hugen/pkg/missions/executor"
	missionsfollowup "github.com/hugr-lab/hugen/pkg/missions/followup"
	"github.com/hugr-lab/hugen/pkg/missions/graph"
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

	// Artifacts is the phase-3 artifact registry manager. Implements
	// both tools.Provider (for the tools.Manager surface) and
	// google.golang.org/adk/artifact.Service (for the A2A / dev-UI
	// path), so the same value flows through every consumer.
	Artifacts *artifacts.Manager

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
	// AbandonCoordinator walks the in-memory DAG and acquires the
	// executor's tickMu — under contention with an in-flight Tick the
	// call can block briefly. Run it off the close-hook caller's
	// goroutine so SessionManager doesn't stall on us. Background
	// ctx is fine: Cancel only writes terminal rows + emits events,
	// and we want it to complete even if the originating request
	// was cancelled.
	sessionMgr.OnSessionClose(func(coordID string) {
		go missionsExec.AbandonCoordinator(context.Background(), coordID)
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

	// Spec 008 — artifact registry. Storage backend is selected by
	// operator config; fs is the day-1 implementation, s3 ships as
	// a stub that registers cleanly but refuses I/O. Manager
	// implements both tools.Provider AND adk artifact.Service —
	// same value flows to the tools.Manager, the A2A path, and the
	// dev-UI without adapter shims.
	registerArtifactBackends()
	storageBackend, err := openArtifactStorage(cfg.Artifacts, logger)
	if err != nil {
		closeOnErr()
		return nil, fmt.Errorf("runtime: open artifact storage: %w", err)
	}
	artManager, err := artifacts.New(artifactsConfigFromOperatorConfig(cfg.Artifacts), artifacts.Deps{
		Querier:         memoryQuerier,
		Storage:         storageBackend,
		SessionEvents:   sessHub,
		Logger:          logger,
		AgentID:         cfg.Identity.ID,
		AgentShort:      cfg.Identity.ShortID,
		EmbedderEnabled: embedderEnabled,
	})
	if err != nil {
		closeOnErr()
		return nil, fmt.Errorf("runtime: build artifact manager: %w", err)
	}
	rt.Artifacts = artManager
	toolsMgr.AddProvider(artManager)

	logger.Info("runtime: internal services registered",
		"providers", []string{
			skills.ServiceName, memory.ServiceName, chatcontext.ServiceName,
			hugen.SubAgentProviderName, missions.ServiceName,
			artifacts.ServiceName,
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

	// Spec 007 US4 — auto-fire one coordinator turn after every
	// graph completion. The marker text + JSON payload are passed
	// to runner.Run as the synthetic user message; ADK records the
	// user_message itself and drives one coordinator turn so
	// SKILL.md branch 8 fires the summary without waiting on the
	// user. AppName/UserID come from the coord session's own
	// metadata so the auto-fired turn uses the same identity ADK
	// saw on the last user-driven turn. Errors are logged and the
	// caller (executor) falls back to leaving the marker for the
	// user's next turn.
	missionsExec.RunOnce = func(
		ctx context.Context,
		coordSessionID string,
		_ graph.CompletionPayload,
	) error {
		// Lifecycle of this goroutine is owned by the executor —
		// e.wg.Add was called before it spawned us. Don't double-
		// count by adding here.
		appName, userID := coordIdentity(sessionMgr, coordSessionID)
		r, rerr := runner.New(runner.Config{
			AppName:        appName,
			Agent:          a,
			SessionService: sessionMgr,
		})
		if rerr != nil {
			return fmt.Errorf("runtime: runonce build runner: %w", rerr)
		}
		// Minimal natural-language nudge — agent_result events
		// already in the transcript carry every mission's outcome,
		// so the LLM has full context already. Some local models
		// return empty when the prompt is verbose; keep RunOnce's
		// trigger short.
		msg := &genai.Content{
			Role: "user",
			Parts: []*genai.Part{{
				Text: "All missions are complete. Summarise the result for me in one short paragraph.",
			}},
		}
		for _, runErr := range r.Run(ctx, userID, coordSessionID, msg, agent.RunConfig{}) {
			if runErr != nil {
				return fmt.Errorf("runtime: runonce: %w", runErr)
			}
		}
		return nil
	}

	bgCtx, bgCancel := context.WithCancel(ctx)
	rt.bgCtx = bgCtx
	rt.bgCancel = bgCancel

	// Hand bgCtx to the executor — its dispatcher goroutines + the
	// completion-summary RunOnce derive runCtx from it, so a Close()
	// that bgCancels actually propagates into in-flight LLM streams
	// (after Stop()'s budget elapses).
	if rt.Missions != nil {
		rt.Missions.SetBaseContext(bgCtx)
	}

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
		// Polite shutdown — gate new mission launches + new RunOnce
		// fan-outs at the executor first, then wait for in-flight
		// goroutines (per-mission dispatchers + completion RunOnce)
		// to finish on the still-live bgCtx. Cancelling bgCtx
		// before this would kill the LLM stream mid-flight; the
		// classifier's consumer loop would also lose its live ctx
		// for hub appends, dropping the freshly-produced events.
		// Budget is generous — LLM responses can take 30s+.
		if r.Missions != nil {
			if err := r.Missions.Stop(90 * time.Second); err != nil && r.logger != nil {
				r.logger.Warn("runtime: missions stop", "err", err)
			}
		}
		// Flush the classifier's queue with a live ctx so events
		// produced just-now by the LLM responses we waited for above
		// actually persist. After bgCancel any queued append uses a
		// dead ctx and the row is lost.
		if r.Classifier != nil {
			if err := r.Classifier.Flush(context.Background(), 30*time.Second); err != nil && r.logger != nil {
				r.logger.Warn("runtime: classifier flush before shutdown", "err", err)
			}
		}

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

// coordIdentity returns the (app_name, user_id) pair carried by the
// coordinator session — used by RunOnce so the auto-fired turn
// matches the identity ADK saw on the last user-driven turn. Falls
// back to safe defaults when the session is unknown so the call
// still succeeds.
func coordIdentity(sm *sessions.Manager, coordSessionID string) (appName, userID string) {
	appName = "hugr-agent"
	userID = "user"
	if sm == nil || coordSessionID == "" {
		return
	}
	sess, err := sm.Session(coordSessionID)
	if err != nil || sess == nil {
		return
	}
	if v := sess.AppName(); v != "" {
		appName = v
	}
	if v := sess.UserID(); v != "" {
		userID = v
	}
	return
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

// artifactBackendsRegistered ensures storage.Register fires exactly
// once per process (storage.Register panics on duplicate; tests
// build multiple Runtimes in a single binary).
var artifactBackendsRegistered sync.Once

// registerArtifactBackends installs fs + s3 factories into the
// storage registry. Idempotent across Runtime constructions.
//
// Per Constitution §III the registry MUST NOT be populated from
// init() in the backend packages — the runtime owns wiring.
func registerArtifactBackends() {
	artifactBackendsRegistered.Do(func() {
		artstorage.Register(artfs.Name, artfs.NewFactory)
		artstorage.Register(arts3.Name, arts3.NewFactory)
	})
}

// openArtifactStorage selects the active backend from operator
// config and returns the live Storage. Logs a clear warning when
// the operator picked the s3 stub so they can see at boot that
// publishes will be refused.
func openArtifactStorage(cfg config.ArtifactsConfig, logger *slog.Logger) (artstorage.Storage, error) {
	switch cfg.Backend {
	case "fs", "":
		s, err := artstorage.Open(artfs.Name, artfs.Config{
			Dir:        cfg.FS.Dir,
			CreateMode: cfg.FS.CreateMode,
		})
		if err != nil {
			return nil, err
		}
		logger.Info("artifacts: storage backend",
			"backend", artfs.Name,
			"dir", cfg.FS.Dir)
		return s, nil
	case "s3":
		s, err := artstorage.Open(arts3.Name, arts3.Config{
			Bucket:  cfg.S3.Bucket,
			Region:  cfg.S3.Region,
			Prefix:  cfg.S3.Prefix,
			RoleARN: cfg.S3.RoleARN,
		})
		if err != nil {
			return nil, err
		}
		logger.Warn("artifacts: storage backend (stub — publishes will be rejected with ErrNotImplemented; switch artifacts.backend back to 'fs' for production)",
			"backend", arts3.Name,
			"bucket", cfg.S3.Bucket,
			"region", cfg.S3.Region)
		return s, nil
	default:
		return nil, fmt.Errorf("runtime: unknown artifacts.backend %q (want fs|s3)", cfg.Backend)
	}
}

// artifactsConfigFromOperatorConfig translates the operator's
// pkg/config.ArtifactsConfig into the manager's package-internal
// Config. Backend-specific keys (cfg.FS.Dir, cfg.S3.*) intentionally
// do NOT cross this boundary — they were already consumed by
// openArtifactStorage. The manager itself never reads them.
func artifactsConfigFromOperatorConfig(cfg config.ArtifactsConfig) artifacts.Config {
	schemaInspect := true
	if cfg.SchemaInspect != nil {
		schemaInspect = *cfg.SchemaInspect
	}
	return artifacts.Config{
		InlineBytesMax:  cfg.InlineBytesMax,
		ADKLoadMaxBytes: cfg.DownloadMaxBytes,
		SchemaInspect:   schemaInspect,
		TTLSessionGrace: int64(cfg.TTLSession.Seconds()),
		TTL7dSeconds:    int64(cfg.TTL7d.Seconds()),
		TTL30dSeconds:   int64(cfg.TTL30d.Seconds()),
	}
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
