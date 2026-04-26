// Package fs is the filesystem storage backend for pkg/artifacts.
// Production-ready on phase 3: bytes are written under a single
// configured directory via atomic temp+rename; LocalPath returns the
// absolute path so artifact_query can hand DuckDB a stable disk
// path.
package fs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hugr-lab/hugen/pkg/artifacts/storage"
)

// Name is the registered backend name. Persisted into
// artifacts.storage_backend at Put time.
const Name = "fs"

// Config configures the filesystem backend. Validated at New time.
type Config struct {
	// Dir is the directory where artifact bytes live. Resolved to an
	// absolute path by New (relative paths resolve against the
	// working directory at boot). Empty is invalid.
	Dir string

	// CreateMode is the directory mode applied at first use. Octal
	// string ("0700", "0750"). Zero falls back to "0700".
	CreateMode string
}

// Backend is the filesystem Storage implementation.
type Backend struct {
	dir     string
	dirMode os.FileMode
}

// New validates cfg, ensures the directory exists, and returns a
// ready Backend. Constructor does NOT enumerate existing files; an
// optional integrity scan is left to the manager (spec 008 / US9).
func New(cfg Config) (storage.Storage, error) {
	if strings.TrimSpace(cfg.Dir) == "" {
		return nil, errors.New("artifacts/storage/fs: empty Dir")
	}
	abs, err := filepath.Abs(cfg.Dir)
	if err != nil {
		return nil, fmt.Errorf("artifacts/storage/fs: resolve dir: %w", err)
	}
	mode, err := parseMode(cfg.CreateMode)
	if err != nil {
		return nil, fmt.Errorf("artifacts/storage/fs: parse create_mode: %w", err)
	}
	if err := os.MkdirAll(abs, mode); err != nil {
		return nil, fmt.Errorf("artifacts/storage/fs: mkdir %s: %w", abs, err)
	}
	return &Backend{dir: abs, dirMode: mode}, nil
}

// NewFactory adapts New to the storage.Factory signature so the
// runtime can register it via storage.Register("fs", fs.NewFactory).
func NewFactory(raw any) (storage.Storage, error) {
	cfg, ok := raw.(Config)
	if !ok {
		return nil, fmt.Errorf("artifacts/storage/fs: factory: expected fs.Config, got %T", raw)
	}
	return New(cfg)
}

// Name implements storage.Storage.
func (b *Backend) Name() string { return Name }

// Put streams src into <dir>/<id>.<ext> via temp + rename.
func (b *Backend) Put(ctx context.Context, hint storage.PutHint, src io.Reader) (storage.ObjectRef, error) {
	if hint.ID == "" {
		return storage.ObjectRef{}, errors.New("artifacts/storage/fs: Put: empty hint.ID")
	}
	if src == nil {
		return storage.ObjectRef{}, errors.New("artifacts/storage/fs: Put: nil src")
	}
	key := keyFor(hint)
	final := filepath.Join(b.dir, key)

	tmp, err := os.CreateTemp(b.dir, "art-*.tmp")
	if err != nil {
		return storage.ObjectRef{}, fmt.Errorf("artifacts/storage/fs: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return storage.ObjectRef{}, fmt.Errorf("artifacts/storage/fs: chmod temp: %w", err)
	}

	if _, err := io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		cleanup()
		return storage.ObjectRef{}, fmt.Errorf("artifacts/storage/fs: write %s: %w", final, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return storage.ObjectRef{}, fmt.Errorf("artifacts/storage/fs: sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return storage.ObjectRef{}, fmt.Errorf("artifacts/storage/fs: close temp: %w", err)
	}
	if err := os.Rename(tmpPath, final); err != nil {
		cleanup()
		return storage.ObjectRef{}, fmt.Errorf("artifacts/storage/fs: rename %s → %s: %w", tmpPath, final, err)
	}

	// Honour ctx cancellation post-write — at this point the bytes
	// are durable, but a cancelled context means the manager won't
	// commit the metadata row, so we leave the file orphaned and let
	// the integrity scan reap it. (Removing eagerly here would be a
	// correctness issue if the manager DID actually commit.)
	if err := ctx.Err(); err != nil {
		return storage.ObjectRef{Backend: Name, Key: key}, fmt.Errorf("artifacts/storage/fs: %w", err)
	}
	return storage.ObjectRef{Backend: Name, Key: key}, nil
}

// Open reads the bytes at ref. ref.Backend MUST equal Name.
func (b *Backend) Open(_ context.Context, ref storage.ObjectRef) (io.ReadCloser, error) {
	if ref.Backend != Name {
		return nil, fmt.Errorf("artifacts/storage/fs: backend mismatch %q (expected %q): %w", ref.Backend, Name, storage.ErrNotFound)
	}
	if ref.Key == "" {
		return nil, fmt.Errorf("artifacts/storage/fs: empty key: %w", storage.ErrNotFound)
	}
	f, err := os.Open(filepath.Join(b.dir, ref.Key))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("artifacts/storage/fs: open %s: %w", ref.Key, storage.ErrNotFound)
		}
		return nil, fmt.Errorf("artifacts/storage/fs: open %s: %w", ref.Key, err)
	}
	return f, nil
}

// Stat returns size + mtime + sniffed content-type.
func (b *Backend) Stat(_ context.Context, ref storage.ObjectRef) (storage.Stat, error) {
	if ref.Backend != Name {
		return storage.Stat{}, fmt.Errorf("artifacts/storage/fs: backend mismatch %q: %w", ref.Backend, storage.ErrNotFound)
	}
	info, err := os.Stat(filepath.Join(b.dir, ref.Key))
	if err != nil {
		if os.IsNotExist(err) {
			return storage.Stat{}, fmt.Errorf("artifacts/storage/fs: stat %s: %w", ref.Key, storage.ErrNotFound)
		}
		return storage.Stat{}, fmt.Errorf("artifacts/storage/fs: stat %s: %w", ref.Key, err)
	}
	ct := mime.TypeByExtension(filepath.Ext(ref.Key))
	return storage.Stat{
		Size:        info.Size(),
		ModTime:     info.ModTime(),
		ContentType: ct,
	}, nil
}

// Delete removes ref's bytes. Idempotent on ErrNotFound.
func (b *Backend) Delete(_ context.Context, ref storage.ObjectRef) error {
	if ref.Backend != Name {
		return fmt.Errorf("artifacts/storage/fs: backend mismatch %q: %w", ref.Backend, storage.ErrNotFound)
	}
	if err := os.Remove(filepath.Join(b.dir, ref.Key)); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("artifacts/storage/fs: remove %s: %w", ref.Key, storage.ErrNotFound)
		}
		return fmt.Errorf("artifacts/storage/fs: remove %s: %w", ref.Key, err)
	}
	return nil
}

// LocalPath returns the absolute path of the stored object so the
// embedded DuckDB engine can read it directly.
func (b *Backend) LocalPath(_ context.Context, ref storage.ObjectRef) (string, bool, error) {
	if ref.Backend != Name {
		return "", false, fmt.Errorf("artifacts/storage/fs: backend mismatch %q: %w", ref.Backend, storage.ErrNotFound)
	}
	p := filepath.Join(b.dir, ref.Key)
	if _, err := os.Stat(p); err != nil {
		if os.IsNotExist(err) {
			return "", false, fmt.Errorf("artifacts/storage/fs: local path %s: %w", ref.Key, storage.ErrNotFound)
		}
		return "", false, fmt.Errorf("artifacts/storage/fs: local path %s: %w", ref.Key, err)
	}
	return p, true, nil
}

// keyFor derives the on-disk file name from the publish hint. Format:
// "<id>.<ext>" where ext is sanitised from PutHint.Type (or "bin"
// when type is unknown / unsafe).
func keyFor(hint storage.PutHint) string {
	ext := sanitiseExt(hint.Type)
	return hint.ID + "." + ext
}

func sanitiseExt(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	if t == "" {
		return "bin"
	}
	// Reject path traversal, dots, slashes, anything but [a-z0-9_].
	for _, r := range t {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '_' {
			return "bin"
		}
	}
	if len(t) > 16 {
		return "bin"
	}
	return t
}

// parseMode parses an octal string like "0700" into an os.FileMode.
// Empty string falls back to 0o700.
func parseMode(s string) (os.FileMode, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0o700, nil
	}
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid octal %q: %w", s, err)
	}
	return os.FileMode(v), nil
}

// Compile-time interface assertion.
var _ storage.Storage = (*Backend)(nil)
