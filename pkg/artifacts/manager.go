package artifacts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	adkartifact "google.golang.org/adk/artifact"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"

	artstore "github.com/hugr-lab/hugen/pkg/artifacts/store"
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

	// EmbedderEnabled toggles the `summary:` argument on
	// insert_artifacts so Hugr embeds description server-side.
	// Threaded through to artifacts/store.Client.
	EmbedderEnabled bool
}

// Config is the package-internal subset of pkg/config.ArtifactsConfig
// the manager actually consumes. Backend-specific config keys
// (cfg.Artifacts.FS / cfg.Artifacts.S3) NEVER reach this struct;
// they're consumed by storage.Open in the runtime wiring layer.
type Config struct {
	// InlineBytesMax caps the size of inline_bytes payloads accepted
	// by artifact_publish.
	InlineBytesMax int64

	// ADKLoadMaxBytes caps the buffer size returned by Manager.Load.
	// Zero falls back to InlineBytesMax. Production runtimes wire the
	// operator's cfg.Artifacts.DownloadMaxBytes here so ADK consumers
	// inherit the same ceiling as the HTTP download endpoint.
	ADKLoadMaxBytes int64

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
	cfg   Config
	deps  Deps
	log   *slog.Logger
	tools []tool.Tool

	// cachedStore is built lazily on first store-facing method call.
	// Single Client per Manager; Manager-side methods funnel through
	// store() to grab it.
	cachedStore *artstoreClient
}

// artstoreClient is an alias to keep the manager's struct field
// definition above package-private without leaking the
// pkg/artifacts/store import path into manager.go's interface.
type artstoreClient = artstore.Client

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

// buildTools returns the slice of unexported artifact tools. The
// slice grows story-by-story: US1 adds artifact_publish; US2 adds
// artifact_info; US3 adds artifact_query; US4 adds artifact_remove +
// artifact_visibility + artifact_list; US9 adds artifact_chain.
func (m *Manager) buildTools() []tool.Tool {
	return []tool.Tool{
		&artifactPublishTool{m: m},
		&artifactInfoTool{m: m},
	}
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

// Save implements adkartifact.Service. The ADK contract is opaque
// to our Visibility / Tags / DerivedFrom / TTL knobs — Save calls
// always land as visibility=self, ttl=session, no tags, no
// derivation. Producers that need richer metadata go through the
// artifact_publish tool instead.
//
// Returns version=1 for every successful Save; phase 3 treats
// artifacts as immutable single-version objects (research §1).
func (m *Manager) Save(ctx context.Context, req *adkartifact.SaveRequest) (*adkartifact.SaveResponse, error) {
	if req == nil {
		return nil, errors.New("artifacts: Save: nil request")
	}
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("artifacts: Save: %w", err)
	}

	// genai.Part may carry inline blob bytes (Part.InlineData) or
	// text (Part.Text). For our purposes both flow through Publish
	// as inline bytes; text becomes a UTF-8 byte slice typed as
	// "txt".
	pubReq := PublishRequest{
		CallerSessionID: req.SessionID,
		Name:            req.FileName,
		Visibility:      VisibilitySelf,
		TTL:             TTLSession,
	}
	switch {
	case req.Part.InlineData != nil && len(req.Part.InlineData.Data) > 0:
		pubReq.Source = PublishSource{InlineBytes: req.Part.InlineData.Data}
		pubReq.Type = typeFromMIME(req.Part.InlineData.MIMEType)
		pubReq.Description = req.FileName
	case req.Part.Text != "":
		pubReq.Source = PublishSource{InlineBytes: []byte(req.Part.Text)}
		pubReq.Type = "txt"
		pubReq.Description = req.FileName
	default:
		return nil, errors.New("artifacts: Save: empty part (need InlineData or Text)")
	}

	if _, err := m.Publish(ctx, pubReq); err != nil {
		return nil, err
	}
	return &adkartifact.SaveResponse{Version: 1}, nil
}

// mimeFromType is the inverse of typeFromMIME — maps our `type`
// enum onto a canonical IANA media type. Returns
// "application/octet-stream" for unrecognised types so HTTP
// Content-Type and ADK genai.Blob.MIMEType always have a value.
func mimeFromType(t string) string {
	switch t {
	case "json":
		return "application/json"
	case "csv":
		return "text/csv"
	case "html":
		return "text/html; charset=utf-8"
	case "txt":
		return "text/plain; charset=utf-8"
	case "md":
		return "text/markdown; charset=utf-8"
	case "svg":
		return "image/svg+xml"
	case "pdf":
		return "application/pdf"
	case "parquet":
		return "application/vnd.apache.parquet"
	default:
		return "application/octet-stream"
	}
}

// typeFromMIME maps a MIME string onto our `type` enum. Returns
// "bin" for unrecognised types so the file extension lookup in the
// fs backend always has a value.
func typeFromMIME(mt string) string {
	switch {
	case mt == "":
		return "bin"
	case mt == "application/json" || mt == "text/json":
		return "json"
	case mt == "text/csv":
		return "csv"
	case mt == "text/html":
		return "html"
	case mt == "text/plain" || mt == "text/markdown":
		return "txt"
	case mt == "image/svg+xml":
		return "svg"
	case mt == "application/pdf":
		return "pdf"
	case mt == "application/x-parquet" || mt == "application/vnd.apache.parquet":
		return "parquet"
	default:
		return "bin"
	}
}

// Load implements adkartifact.Service. Resolves the artifact id from
// (sessionID, fileName), opens the bytes through OpenReader, and
// returns a buffered genai.Part. The buffer is capped at
// cfg.ADKLoadMaxBytes (defaults to InlineBytesMax when unset) so
// runaway loads can't blow up agent memory.
func (m *Manager) Load(ctx context.Context, req *adkartifact.LoadRequest) (*adkartifact.LoadResponse, error) {
	if req == nil {
		return nil, errors.New("artifacts: Load: nil request")
	}
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("artifacts: Load: %w", err)
	}
	rec, found, err := m.store().GetByName(ctx, req.SessionID, req.FileName)
	if err != nil {
		return nil, fmt.Errorf("artifacts: Load: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("%w: %s/%s", ErrUnknownArtifact, req.SessionID, req.FileName)
	}
	rc, _, err := m.OpenReader(ctx, req.SessionID, rec.ID)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	limit := m.cfg.ADKLoadMaxBytes
	if limit <= 0 {
		limit = m.cfg.InlineBytesMax
	}
	var reader io.Reader = rc
	if limit > 0 {
		reader = io.LimitReader(rc, limit+1)
	}
	buf, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("artifacts: Load: read: %w", err)
	}
	if limit > 0 && int64(len(buf)) > limit {
		return nil, fmt.Errorf("artifacts: Load: %s exceeds ADK load cap (%d bytes)", rec.ID, limit)
	}
	mt := mimeFromType(rec.Type)
	return &adkartifact.LoadResponse{Part: &genai.Part{
		InlineData: &genai.Blob{
			Data:        buf,
			MIMEType:    mt,
			DisplayName: rec.Name,
		},
	}}, nil
}

// Delete implements adkartifact.Service. Resolves id by name and
// routes to the domain Remove method (which lands fully in T053 /
// US4). Foundation phase still returns the not-implemented envelope
// from Remove.
func (m *Manager) Delete(ctx context.Context, req *adkartifact.DeleteRequest) error {
	if req == nil {
		return errors.New("artifacts: Delete: nil request")
	}
	if err := req.Validate(); err != nil {
		return fmt.Errorf("artifacts: Delete: %w", err)
	}
	rec, found, err := m.store().GetByName(ctx, req.SessionID, req.FileName)
	if err != nil {
		return fmt.Errorf("artifacts: Delete: %w", err)
	}
	if !found {
		return fmt.Errorf("%w: %s/%s", ErrUnknownArtifact, req.SessionID, req.FileName)
	}
	return m.Remove(ctx, req.SessionID, rec.ID)
}

// List implements adkartifact.Service. Returns the names visible to
// (sessionID). Routes through ListVisible (T051 / US4); foundation
// phase still returns the not-implemented envelope from ListVisible.
func (m *Manager) List(ctx context.Context, req *adkartifact.ListRequest) (*adkartifact.ListResponse, error) {
	if req == nil {
		return nil, errors.New("artifacts: List: nil request")
	}
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("artifacts: List: %w", err)
	}
	refs, err := m.ListVisible(ctx, req.SessionID, ListFilter{})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(refs))
	for _, r := range refs {
		names = append(names, r.Name)
	}
	return &adkartifact.ListResponse{FileNames: names}, nil
}

// Versions implements adkartifact.Service. Phase 3 treats artifacts
// as immutable single-version objects — every Versions call returns
// `[1]` for known artifacts.
func (m *Manager) Versions(ctx context.Context, req *adkartifact.VersionsRequest) (*adkartifact.VersionsResponse, error) {
	if req == nil {
		return nil, errors.New("artifacts: Versions: nil request")
	}
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("artifacts: Versions: %w", err)
	}
	_, found, err := m.store().GetByName(ctx, req.SessionID, req.FileName)
	if err != nil {
		return nil, fmt.Errorf("artifacts: Versions: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("%w: %s/%s", ErrUnknownArtifact, req.SessionID, req.FileName)
	}
	return &adkartifact.VersionsResponse{Versions: []int64{1}}, nil
}

// GetArtifactVersion implements adkartifact.Service.
func (m *Manager) GetArtifactVersion(ctx context.Context, req *adkartifact.GetArtifactVersionRequest) (*adkartifact.GetArtifactVersionResponse, error) {
	if req == nil {
		return nil, errors.New("artifacts: GetArtifactVersion: nil request")
	}
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("artifacts: GetArtifactVersion: %w", err)
	}
	rec, found, err := m.store().GetByName(ctx, req.SessionID, req.FileName)
	if err != nil {
		return nil, fmt.Errorf("artifacts: GetArtifactVersion: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("%w: %s/%s", ErrUnknownArtifact, req.SessionID, req.FileName)
	}
	return &adkartifact.GetArtifactVersionResponse{
		ArtifactVersion: &adkartifact.ArtifactVersion{
			Version:    1,
			MimeType:   mimeFromType(rec.Type),
			CreateTime: float64(rec.CreatedAt.Unix()),
		},
	}, nil
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
