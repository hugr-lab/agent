package config

import "time"

// ArtifactsConfig bundles the operator knobs for the persistent
// artifact registry (spec 008). Selects the active storage backend
// and tunes TTL + cleanup + download behaviour. Backend-specific
// substructs (FS, S3) are passed by the runtime to the backend's
// constructor; the artifacts manager itself never reads them.
type ArtifactsConfig struct {
	// Backend selects the active storage backend. One of "fs", "s3".
	// Unknown values fail fast at runtime construction. Zero falls
	// back to "fs".
	Backend string `mapstructure:"backend"`

	// FS configures the filesystem storage backend (production-ready).
	FS FSBackendConfig `mapstructure:"fs"`

	// S3 configures the object-store backend. Phase 3 ships only the
	// stub — fields are validated but every I/O method returns
	// ErrNotImplemented until the post-008 spec replaces the stub.
	S3 S3BackendConfig `mapstructure:"s3"`

	// TTLSession is the grace window applied after a creator session
	// reaches `completed` before its `session`-TTL artifacts become
	// eligible for cleanup. Zero falls back to 24h.
	TTLSession time.Duration `mapstructure:"ttl_session"`

	// TTL7d is the absolute age at which a `7d`-TTL artifact becomes
	// eligible for cleanup. Zero falls back to 168h.
	TTL7d time.Duration `mapstructure:"ttl_7d"`

	// TTL30d is the absolute age at which a `30d`-TTL artifact
	// becomes eligible for cleanup. Zero falls back to 720h.
	TTL30d time.Duration `mapstructure:"ttl_30d"`

	// CleanupSchedule is the cron expression driving the
	// scheduler.Cron("artifacts-cleanup", …) task. Zero falls back to
	// "0 3 * * *" (daily at 03:00 local).
	CleanupSchedule string `mapstructure:"cleanup_schedule"`

	// DownloadMaxBytes caps the size of an artifact streamed via
	// /admin/artifacts/{id}. Larger artifacts return HTTP 413. Zero
	// falls back to 256 MiB.
	DownloadMaxBytes int64 `mapstructure:"download_max_bytes"`

	// InlineBytesMax caps the size of the `inline_bytes` payload
	// accepted by artifact_publish. Zero falls back to 1 MiB.
	InlineBytesMax int64 `mapstructure:"inline_bytes_max"`

	// DownloadWriteChunk is the per-chunk write deadline boundary
	// applied during streaming on /admin/artifacts/{id}. Zero falls
	// back to 1 MiB.
	DownloadWriteChunk int64 `mapstructure:"download_write_chunk"`

	// DownloadWriteTimeout is the per-chunk write timeout applied via
	// http.ResponseController.SetWriteDeadline. Zero falls back to
	// 60s.
	DownloadWriteTimeout time.Duration `mapstructure:"download_write_timeout"`

	// SchemaInspect controls whether Manager.Publish runs a
	// SELECT COUNT(*) probe on tabular sources. Always-true falls
	// back to true (publishing introspection is cheap on small
	// Parquet/CSV files; operators who push GB-scale tabular sources
	// can flip it off).
	SchemaInspect *bool `mapstructure:"schema_inspect"`

	// UploadDefaultVisibility is the visibility applied when ADK's
	// runner auto-publishes a user-uploaded blob via the artifact.Service
	// shim (see runner.go::appendMessageToSession). Operators who want
	// uploads to stay session-scoped can set "self"; the default "user"
	// makes them visible across the whole session graph for that user.
	// One of: "self" | "parent" | "graph" | "user". Zero falls back to
	// "user".
	UploadDefaultVisibility string `mapstructure:"upload_default_visibility"`

	// UploadDefaultTTL is the TTL applied to ADK-runner-auto-published
	// uploads. One of: "session" | "7d" | "30d" | "permanent". Zero
	// falls back to "7d".
	UploadDefaultTTL string `mapstructure:"upload_default_ttl"`
}

// FSBackendConfig configures the filesystem storage backend.
type FSBackendConfig struct {
	// Dir is the directory where artifact bytes live. Resolved
	// relative to the working directory if not absolute. Zero falls
	// back to "data/artifacts".
	Dir string `mapstructure:"dir"`

	// CreateMode is the dir mode applied at first use. Octal string
	// (`0700`, `0750`). Zero falls back to "0700".
	CreateMode string `mapstructure:"create_mode"`
}

// S3BackendConfig configures the (stubbed) S3 storage backend.
// Validated at runtime construction; not used otherwise on phase 3.
type S3BackendConfig struct {
	Bucket  string `mapstructure:"bucket"`
	Region  string `mapstructure:"region"`
	Prefix  string `mapstructure:"prefix"`
	RoleARN string `mapstructure:"role_arn"`
}

// applyArtifactsDefaults fills zero-valued ArtifactsConfig fields
// with their documented defaults. Called from config.LoadLocal after
// YAML unmarshal so operator overrides survive but missing keys land
// on safe values.
func applyArtifactsDefaults(c *ArtifactsConfig) {
	if c.Backend == "" {
		c.Backend = "fs"
	}
	if c.FS.Dir == "" {
		c.FS.Dir = "data/artifacts"
	}
	if c.FS.CreateMode == "" {
		c.FS.CreateMode = "0700"
	}
	if c.TTLSession == 0 {
		c.TTLSession = 24 * time.Hour
	}
	if c.TTL7d == 0 {
		c.TTL7d = 168 * time.Hour
	}
	if c.TTL30d == 0 {
		c.TTL30d = 720 * time.Hour
	}
	if c.CleanupSchedule == "" {
		c.CleanupSchedule = "0 3 * * *"
	}
	if c.DownloadMaxBytes == 0 {
		c.DownloadMaxBytes = 256 << 20 // 256 MiB
	}
	if c.InlineBytesMax == 0 {
		c.InlineBytesMax = 1 << 20 // 1 MiB
	}
	if c.DownloadWriteChunk == 0 {
		c.DownloadWriteChunk = 1 << 20 // 1 MiB
	}
	if c.DownloadWriteTimeout == 0 {
		c.DownloadWriteTimeout = 60 * time.Second
	}
	if c.SchemaInspect == nil {
		t := true
		c.SchemaInspect = &t
	}
	if c.UploadDefaultVisibility == "" {
		c.UploadDefaultVisibility = "user"
	}
	if c.UploadDefaultTTL == "" {
		c.UploadDefaultTTL = "7d"
	}
}
