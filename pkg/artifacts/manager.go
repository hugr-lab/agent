package artifacts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	adkartifact "google.golang.org/adk/artifact"
	"google.golang.org/adk/tool"

	"github.com/hugr-lab/hugen/pkg/artifacts/storage"
	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
	"github.com/hugr-lab/query-engine/types"
)

// ServiceName is the provider name *Manager registers under in
// tools.Manager. Skill files reference it via
// `providers: [{provider: _artifacts}]`.
const ServiceName = "_artifacts"

// errSessionEvents is the local-package shorthand for the session
// event writer. Defined as an interface so tests can substitute a
// fake without standing up a full *sessstore.Client.
type sessionEventWriter interface {
	AppendEventWithSummary(ctx context.Context, ev sessstore.Event, summary string) (string, error)
}

// Deps bundles the manager's external dependencies. Constructed once
// at runtime startup; passed in through New.
type Deps struct {
	// Querier is the hub.db GraphQL surface. Required.
	Querier types.Querier

	// Storage is the active backend that owns artifact bytes.
	// Required.
	Storage storage.Storage

	// SessionEvents writes lifecycle events
	// (artifact_published / granted / removed) onto the producer's
	// session. Required.
	SessionEvents sessionEventWriter

	// Logger is optional — defaults to slog.Default() when nil.
	Logger *slog.Logger

	// AgentID and AgentShort scope the manager to a single agent.
	// Required.
	AgentID    string
	AgentShort string
}

// Config is the package-internal subset of pkg/config.ArtifactsConfig
// the manager actually consumes. Backend-specific config keys
// (cfg.Artifacts.FS / cfg.Artifacts.S3) NEVER reach this struct;
// they're consumed by storage.Open in the runtime wiring layer.
type Config struct {
	// InlineBytesMax caps the size of inline_bytes payloads accepted
	// by artifact_publish.
	InlineBytesMax int64

	// SchemaInspect controls whether Manager.Publish probes tabular
	// sources for row/col counts via DuckDB DESCRIBE / COUNT(*).
	SchemaInspect bool

	// TTLSession / TTL7d / TTL30d drive cleanup eligibility.
	TTLSessionGrace int64 // seconds
	TTL7dSeconds    int64 // seconds
	TTL30dSeconds   int64 // seconds
}

// Manager is the public surface of the artifact registry. Implements
// tools.Provider (Name / Tools) AND ADK artifact.Service
// (Save / Load / Delete / List / Versions / GetArtifactVersion)
// directly — same pattern as pkg/memory/service.go and
// pkg/missions/spawn.go.
type Manager struct {
	cfg     Config
	deps    Deps
	log     *slog.Logger
	tools   []tool.Tool
}

// New constructs a Manager, validates required deps, and pre-builds
// the tool slice. Returns an error if any required dep is missing —
// runtime construction fails fast.
func New(cfg Config, deps Deps) (*Manager, error) {
	if deps.Querier == nil {
		return nil, errors.New("artifacts: New: Querier required")
	}
	if deps.Storage == nil {
		return nil, errors.New("artifacts: New: Storage required")
	}
	if deps.SessionEvents == nil {
		return nil, errors.New("artifacts: New: SessionEvents required")
	}
	if deps.AgentID == "" {
		return nil, errors.New("artifacts: New: AgentID required")
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	m := &Manager{
		cfg:  cfg,
		deps: deps,
		log:  logger,
	}
	// Tool slice is built once here (each tool keeps a *Manager
	// back-reference). Returned verbatim by Tools(). For the phase-3
	// foundation slice the slice is empty; US1+ tasks fill it in
	// via buildTools().
	m.tools = m.buildTools()
	return m, nil
}

// Name implements tools.Provider.
func (m *Manager) Name() string { return ServiceName }

// Tools implements tools.Provider.
func (m *Manager) Tools() []tool.Tool { return m.tools }

// buildTools returns the slice of unexported artifact tools. Empty
// in this commit; populated by US1+ tasks per
// specs/008-artifact-registry/contracts/artifact-tools.md.
func (m *Manager) buildTools() []tool.Tool {
	return nil
}

// AgentID returns the scope the Manager was constructed for.
func (m *Manager) AgentID() string { return m.deps.AgentID }

// ─────────────────────────────────────────────────────────────────
// Domain methods (Publish / Remove / WidenVisibility / ListVisible /
// Info / Chain / LocalPathFor / OpenReader / AddGrant / Cleanup)
//
// These are stub-bodied in the foundation slice; bodies land in
// US1..US7 / US9 per specs/008-artifact-registry/tasks.md. Manager
// already has the cfg + deps it needs to flesh them out without
// further plumbing.
// ─────────────────────────────────────────────────────────────────

// Publish stub; full body lands in T026 (US1).
func (m *Manager) Publish(ctx context.Context, _ PublishRequest) (ArtifactRef, error) {
	_ = ctx
	return ArtifactRef{}, errNotImplementedYet("Publish", "T026 / US1")
}

// Remove stub; full body lands in T053 (US4).
func (m *Manager) Remove(ctx context.Context, _ string, _ string) error {
	_ = ctx
	return errNotImplementedYet("Remove", "T053 / US4")
}

// WidenVisibility stub; full body lands in T048 (US4).
func (m *Manager) WidenVisibility(ctx context.Context, _ string, _ string, _ Visibility, _ *GrantTarget) error {
	_ = ctx
	return errNotImplementedYet("WidenVisibility", "T048 / US4")
}

// ListVisible stub; full body lands in T051 (US4).
func (m *Manager) ListVisible(ctx context.Context, _ string, _ ListFilter) ([]ArtifactRef, error) {
	_ = ctx
	return nil, errNotImplementedYet("ListVisible", "T051 / US4")
}

// Info stub; full body lands in T036 (US2).
func (m *Manager) Info(ctx context.Context, _ string, _ string) (ArtifactDetail, error) {
	_ = ctx
	return ArtifactDetail{}, errNotImplementedYet("Info", "T036 / US2")
}

// Chain stub; full body lands in T075 (US9).
func (m *Manager) Chain(ctx context.Context, _ string, _ string) ([]ArtifactRef, error) {
	_ = ctx
	return nil, errNotImplementedYet("Chain", "T075 / US9")
}

// LocalPathFor stub; full body lands in T044 (US3).
func (m *Manager) LocalPathFor(ctx context.Context, _ string, _ string) (string, error) {
	_ = ctx
	return "", errNotImplementedYet("LocalPathFor", "T044 / US3")
}

// OpenReader stub; full body lands in T034 (US2).
func (m *Manager) OpenReader(ctx context.Context, _ string, _ string) (io.ReadCloser, Stat, error) {
	_ = ctx
	return nil, Stat{}, errNotImplementedYet("OpenReader", "T034 / US2")
}

// AddGrant stub; full body lands in T049 (US4) / T063 (US6).
func (m *Manager) AddGrant(ctx context.Context, _ string, _ string, _ string, _ string) error {
	_ = ctx
	return errNotImplementedYet("AddGrant", "T049 / US4")
}

// Cleanup stub; full body lands in T068 (US7).
func (m *Manager) Cleanup(ctx context.Context) (int, error) {
	_ = ctx
	return 0, errNotImplementedYet("Cleanup", "T068 / US7")
}

// ─────────────────────────────────────────────────────────────────
// ADK artifact.Service implementation
//
// Six methods (Save / Load / Delete / List / Versions /
// GetArtifactVersion). On the foundation slice each returns
// ErrNotImplemented; US1/US2 wire them onto the domain methods
// above (Publish / OpenReader / Remove / ListVisible).
// ─────────────────────────────────────────────────────────────────

// Save implements adkartifact.Service.
func (m *Manager) Save(ctx context.Context, req *adkartifact.SaveRequest) (*adkartifact.SaveResponse, error) {
	_ = ctx
	if req == nil {
		return nil, errors.New("artifacts: Save: nil request")
	}
	return nil, errNotImplementedYet("Save", "T028 / US1")
}

// Load implements adkartifact.Service.
func (m *Manager) Load(ctx context.Context, req *adkartifact.LoadRequest) (*adkartifact.LoadResponse, error) {
	_ = ctx
	if req == nil {
		return nil, errors.New("artifacts: Load: nil request")
	}
	return nil, errNotImplementedYet("Load", "T035 / US2")
}

// Delete implements adkartifact.Service.
func (m *Manager) Delete(ctx context.Context, req *adkartifact.DeleteRequest) error {
	_ = ctx
	if req == nil {
		return errors.New("artifacts: Delete: nil request")
	}
	return errNotImplementedYet("Delete", "T055 / US4")
}

// List implements adkartifact.Service.
func (m *Manager) List(ctx context.Context, req *adkartifact.ListRequest) (*adkartifact.ListResponse, error) {
	_ = ctx
	if req == nil {
		return nil, errors.New("artifacts: List: nil request")
	}
	return nil, errNotImplementedYet("List", "T055 / US4")
}

// Versions implements adkartifact.Service. Phase 3 treats artifacts
// as immutable single-version objects — every Versions call returns
// `[1]` for known artifacts. Implemented as a thin shim once Info
// lands in US2 (T036). For the foundation slice, returns the
// not-implemented envelope.
func (m *Manager) Versions(ctx context.Context, req *adkartifact.VersionsRequest) (*adkartifact.VersionsResponse, error) {
	_ = ctx
	if req == nil {
		return nil, errors.New("artifacts: Versions: nil request")
	}
	return nil, errNotImplementedYet("Versions", "T036 / US2")
}

// GetArtifactVersion implements adkartifact.Service.
func (m *Manager) GetArtifactVersion(ctx context.Context, req *adkartifact.GetArtifactVersionRequest) (*adkartifact.GetArtifactVersionResponse, error) {
	_ = ctx
	if req == nil {
		return nil, errors.New("artifacts: GetArtifactVersion: nil request")
	}
	return nil, errNotImplementedYet("GetArtifactVersion", "T036 / US2")
}

// Compile-time interface assertions.
var (
	_ adkartifact.Service = (*Manager)(nil)
)

// errNotImplementedYet returns a clear error pointing the next
// implementer at the right task in tasks.md. Distinct from the
// storage.ErrNotImplemented sentinel used for the s3 stub.
func errNotImplementedYet(method, where string) error {
	return fmt.Errorf("artifacts.%s: not yet implemented (see tasks.md %s)", method, where)
}
