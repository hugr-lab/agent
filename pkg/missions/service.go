package missions

import (
	"google.golang.org/adk/tool"

	"github.com/hugr-lab/hugen/pkg/missions/executor"
	"github.com/hugr-lab/hugen/pkg/missions/planner"
	"github.com/hugr-lab/hugen/pkg/sessions"
	"github.com/hugr-lab/hugen/pkg/skills"
)

// ServiceName is the tools.Manager provider key for the mission-graph
// toolset. Skills that want mission_plan / mission_status declare
// `providers: - name: <local> / provider: _mission_tools` in their
// frontmatter — same pattern as `_memory`, `_context`, `_system`.
const ServiceName = "_mission_tools"

// Service is the mission-graph tools.Provider. One instance per
// runtime; tools close over *Service and resolve the current session
// from tool.Context at Run time.
type Service struct {
	planner  *planner.Planner
	executor *executor.Executor
	sessions *sessions.Manager
	skills   skills.Manager

	tools []tool.Tool
}

// Config bundles the Service's construction deps.
type Config struct {
	Planner  *planner.Planner
	Executor *executor.Executor
	Sessions *sessions.Manager
	Skills   skills.Manager
}

// NewService builds the mission-graph tools.Provider. Returned Tools
// order is stable for deterministic Snapshot rendering.
func NewService(cfg Config) *Service {
	svc := &Service{
		planner:  cfg.Planner,
		executor: cfg.Executor,
		sessions: cfg.Sessions,
		skills:   cfg.Skills,
	}
	svc.tools = []tool.Tool{
		&missionPlanTool{svc: svc},
		&missionStatusTool{svc: svc},
	}
	return svc
}

// Name implements tools.Provider.
func (s *Service) Name() string { return ServiceName }

// Tools implements tools.Provider.
func (s *Service) Tools() []tool.Tool { return s.tools }
