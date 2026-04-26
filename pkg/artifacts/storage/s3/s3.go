// Package s3 is the S3-style object-store backend stub. Phase 3
// ships the constructor + Storage interface implementation; every
// I/O method returns storage.ErrNotImplemented. Wiring is in place
// so an operator can flip artifacts.backend: s3 in config and the
// agent boots — only Put/Open/Stat/Delete will refuse.
//
// Importantly, this package does NOT import any AWS SDK
// (Constitution §V Minimal Dependencies). The post-008 spec replaces
// the stub and adds aws-sdk-go-v2 via an ADR at that time.
package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/hugr-lab/hugen/pkg/artifacts/storage"
)

// Name is the registered backend name. Persisted into
// artifacts.storage_backend at Put time.
const Name = "s3"

// Config configures the (stubbed) S3 backend. Validated at New time.
type Config struct {
	Bucket  string
	Region  string
	Prefix  string
	RoleARN string
}

// Backend implements storage.Storage. All I/O methods return
// ErrNotImplemented; LocalPath returns ("", false, nil) which is the
// legitimate "this backend does not expose a local path" response.
type Backend struct {
	cfg Config
}

// New validates the config and returns the stub.
func New(cfg Config) (storage.Storage, error) {
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, errors.New("artifacts/storage/s3: empty Bucket")
	}
	if strings.TrimSpace(cfg.Region) == "" {
		return nil, errors.New("artifacts/storage/s3: empty Region")
	}
	return &Backend{cfg: cfg}, nil
}

// NewFactory adapts New to the storage.Factory signature so the
// runtime can register it via storage.Register("s3", s3.NewFactory).
func NewFactory(raw any) (storage.Storage, error) {
	cfg, ok := raw.(Config)
	if !ok {
		return nil, fmt.Errorf("artifacts/storage/s3: factory: expected s3.Config, got %T", raw)
	}
	return New(cfg)
}

// Name implements storage.Storage.
func (b *Backend) Name() string { return Name }

// Put returns ErrNotImplemented. Manager surfaces this as a tool
// envelope: "storage backend 's3' not implemented; switch
// artifacts.backend back to 'fs'".
func (b *Backend) Put(_ context.Context, _ storage.PutHint, _ io.Reader) (storage.ObjectRef, error) {
	return storage.ObjectRef{}, fmt.Errorf("artifacts/storage/s3: Put: %w", storage.ErrNotImplemented)
}

// Open returns ErrNotImplemented.
func (b *Backend) Open(_ context.Context, _ storage.ObjectRef) (io.ReadCloser, error) {
	return nil, fmt.Errorf("artifacts/storage/s3: Open: %w", storage.ErrNotImplemented)
}

// Stat returns ErrNotImplemented.
func (b *Backend) Stat(_ context.Context, _ storage.ObjectRef) (storage.Stat, error) {
	return storage.Stat{}, fmt.Errorf("artifacts/storage/s3: Stat: %w", storage.ErrNotImplemented)
}

// Delete returns ErrNotImplemented.
func (b *Backend) Delete(_ context.Context, _ storage.ObjectRef) error {
	return fmt.Errorf("artifacts/storage/s3: Delete: %w", storage.ErrNotImplemented)
}

// LocalPath returns ("", false, nil) — the legitimate "no local
// path" response, NOT an error. Manager treats `ok=false` as
// "artifact_query is unavailable for this artifact" without
// aborting the calling mission.
func (b *Backend) LocalPath(_ context.Context, _ storage.ObjectRef) (string, bool, error) {
	return "", false, nil
}

// Compile-time interface assertion.
var _ storage.Storage = (*Backend)(nil)
