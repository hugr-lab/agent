package artifacts

import (
	"errors"
	"io"
	"time"
)

// Visibility classifies who can see an artifact. Values are
// strictly ordered for the widening rule: self ⊂ parent ⊂ graph ⊂
// user.
type Visibility string

const (
	VisibilitySelf   Visibility = "self"   // creator session only
	VisibilityParent Visibility = "parent" // creator + direct parent
	VisibilityGraph  Visibility = "graph"  // any session in same coord graph
	VisibilityUser   Visibility = "user"   // world (admin endpoint)
)

// Order returns the strict-widening rank of v. Higher = wider.
// Returns -1 for unrecognised values so callers can detect
// validation failures without panicking.
func (v Visibility) Order() int {
	switch v {
	case VisibilitySelf:
		return 0
	case VisibilityParent:
		return 1
	case VisibilityGraph:
		return 2
	case VisibilityUser:
		return 3
	default:
		return -1
	}
}

// CanWidenTo reports whether v can be widened to target without
// violating the strict-widening rule. False when target is narrower
// or equal (no-op widens are not errors at the manager but they're
// also not "widens" — they short-circuit).
func (v Visibility) CanWidenTo(target Visibility) bool {
	a, b := v.Order(), target.Order()
	return a >= 0 && b >= 0 && b > a
}

// IsValid reports whether v is one of the four recognised
// visibility levels.
func (v Visibility) IsValid() bool { return v.Order() >= 0 }

// TTL classifies an artifact's eligibility for the cleanup pass.
type TTL string

const (
	TTLSession   TTL = "session"
	TTL7d        TTL = "7d"
	TTL30d       TTL = "30d"
	TTLPermanent TTL = "permanent"
)

// IsValid reports whether t is one of the four recognised TTL
// classes.
func (t TTL) IsValid() bool {
	switch t {
	case TTLSession, TTL7d, TTL30d, TTLPermanent:
		return true
	}
	return false
}

// ArtifactRef is the lightweight reference returned by listings
// and tool envelopes. Carries identity + scope + size; full
// metadata (file_schema, derivation chain) lives on
// ArtifactDetail.
type ArtifactRef struct {
	ID                string     `json:"id"`
	Name              string     `json:"name"`
	Type              string     `json:"type"`
	Visibility        Visibility `json:"visibility"`
	SizeBytes         int64      `json:"size_bytes,omitempty"`
	Tags              []string   `json:"tags,omitempty"`
	CreatedAt         time.Time  `json:"created_at,omitempty"`
	DistanceToQuery   *float64   `json:"distance_to_query,omitempty"`
	StorageBackend    string     `json:"storage_backend,omitempty"`
}

// ArtifactDetail is the full metadata view returned by Info /
// artifact_info. Adds tabular schema + lineage + derivation parent.
type ArtifactDetail struct {
	ArtifactRef
	Description      string         `json:"description"`
	OriginalPath     string         `json:"original_path,omitempty"`
	SessionID        string         `json:"session_id,omitempty"`
	MissionSessionID string         `json:"mission_session_id,omitempty"`
	DerivedFrom      string         `json:"derived_from,omitempty"`
	RowCount         *int64         `json:"row_count,omitempty"`
	ColCount         *int           `json:"col_count,omitempty"`
	FileSchema       map[string]any `json:"file_schema,omitempty"`
	TTL              TTL            `json:"ttl"`
}

// Stat is a lightweight wrapper over storage.Stat for callers that
// don't need to import the storage subpackage.
type Stat struct {
	Size        int64
	ModTime     time.Time
	ContentType string
}

// GrantTarget identifies a (agent, session) pair receiving an
// explicit grant via WidenVisibility(target: ...).
type GrantTarget struct {
	AgentID   string
	SessionID string
}

// PublishRequest is the input to Manager.Publish. Either Source.Path
// or Source.InlineBytes is set, never both.
type PublishRequest struct {
	CallerSessionID string
	Source          PublishSource
	Name            string
	Type            string
	Description     string
	Visibility      Visibility
	Tags            []string
	DerivedFrom     string
	TTL             TTL
}

// PublishSource is the bytes input for a publish. Exactly one of
// Path or InlineBytes is set.
type PublishSource struct {
	Path        string
	InlineBytes []byte
}

// HasPath reports whether the source is a filesystem path.
func (p PublishSource) HasPath() bool { return p.Path != "" }

// HasInline reports whether the source is in-memory bytes.
func (p PublishSource) HasInline() bool { return len(p.InlineBytes) > 0 }

// ListFilter is the input to Manager.ListVisible /
// artifact_list. All fields optional; a zero filter returns the
// caller's most-recent visible artifacts.
type ListFilter struct {
	Tags   []string
	Type   string
	Search string
	Limit  int
}

// Sentinel errors.
var (
	ErrUnknownArtifact         = errors.New("artifacts: unknown artifact")
	ErrDescriptionRequired     = errors.New("artifacts: description required")
	ErrSourceAmbiguous         = errors.New("artifacts: exactly one of path or inline_bytes must be set")
	ErrInlineBytesTooLarge     = errors.New("artifacts: inline_bytes exceeds InlineBytesMax")
	ErrInvalidVisibility       = errors.New("artifacts: invalid visibility")
	ErrInvalidTTL              = errors.New("artifacts: invalid ttl")
	ErrVisibilityNarrowing     = errors.New("artifacts: visibility can only be widened")
	ErrNotCoordinator          = errors.New("artifacts: only the coordinator may invoke this action")
	ErrNotAuthorisedToRemove   = errors.New("artifacts: caller is not authorised to remove this artifact")
	ErrLocalPathUnavailable    = errors.New("artifacts: backend does not expose a local path; download via artifact_info")
	ErrUnregisteredBackend     = errors.New("artifacts: artifact's storage backend is not currently registered")
)

// nopCloser wraps an io.Reader as an io.ReadCloser. Used by callers
// that want to feed an inline-bytes source into Manager.Publish
// through the same Storage.Put path that handles file-backed
// sources.
type nopCloser struct{ io.Reader }

func (nopCloser) Close() error { return nil }
