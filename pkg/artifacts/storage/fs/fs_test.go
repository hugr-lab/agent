package fs_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hugr-lab/hugen/pkg/artifacts/storage"
	"github.com/hugr-lab/hugen/pkg/artifacts/storage/fs"
)

func TestFS_PutOpenStatLocalPathDelete(t *testing.T) {
	dir := t.TempDir()
	be, err := fs.New(fs.Config{Dir: dir, CreateMode: "0700"})
	require.NoError(t, err)
	require.Equal(t, "fs", be.Name())

	ctx := context.Background()
	body := []byte("hello, world")
	hint := storage.PutHint{ID: "art_ag01_1234_aa", Name: "greeting", Type: "txt", SizeHint: int64(len(body))}

	ref, err := be.Put(ctx, hint, bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, "fs", ref.Backend)
	require.True(t, strings.HasSuffix(ref.Key, ".txt"), "expected txt extension, got %q", ref.Key)
	require.True(t, strings.HasPrefix(ref.Key, hint.ID), "expected key to start with id, got %q", ref.Key)

	// Open returns the same bytes.
	rc, err := be.Open(ctx, ref)
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, rc.Close())
	require.NoError(t, err)
	assert.Equal(t, body, got)

	// Stat reports size and a sniffed content-type ('.txt' → text/plain).
	stat, err := be.Stat(ctx, ref)
	require.NoError(t, err)
	assert.Equal(t, int64(len(body)), stat.Size)
	assert.Contains(t, stat.ContentType, "text/plain")

	// LocalPath resolves to the absolute path, file readable.
	lp, ok, err := be.LocalPath(ctx, ref)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, filepath.Join(dir, ref.Key), lp)
	_, err = os.Stat(lp)
	require.NoError(t, err)

	// Delete removes the file.
	require.NoError(t, be.Delete(ctx, ref))
	_, err = be.Stat(ctx, ref)
	require.ErrorIs(t, err, storage.ErrNotFound)

	// Delete is idempotent on ErrNotFound — second call returns the
	// sentinel, manager treats it as success.
	err = be.Delete(ctx, ref)
	require.ErrorIs(t, err, storage.ErrNotFound)
}

func TestFS_BackendMismatch(t *testing.T) {
	be, err := fs.New(fs.Config{Dir: t.TempDir()})
	require.NoError(t, err)

	bad := storage.ObjectRef{Backend: "s3", Key: "anything"}
	ctx := context.Background()

	_, err = be.Open(ctx, bad)
	require.ErrorIs(t, err, storage.ErrNotFound)

	_, err = be.Stat(ctx, bad)
	require.ErrorIs(t, err, storage.ErrNotFound)

	require.ErrorIs(t, be.Delete(ctx, bad), storage.ErrNotFound)

	_, ok, err := be.LocalPath(ctx, bad)
	require.False(t, ok)
	require.ErrorIs(t, err, storage.ErrNotFound)
}

func TestFS_BinExtensionFallback(t *testing.T) {
	dir := t.TempDir()
	be, err := fs.New(fs.Config{Dir: dir})
	require.NoError(t, err)

	ctx := context.Background()
	ref, err := be.Put(ctx, storage.PutHint{ID: "art_ag01_1_bb"}, strings.NewReader("x"))
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(ref.Key, ".bin"), "empty type should fall back to .bin, got %q", ref.Key)

	// Path-traversal-style type sanitised → .bin.
	ref2, err := be.Put(ctx, storage.PutHint{ID: "art_ag01_2_cc", Type: "../etc/passwd"}, strings.NewReader("x"))
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(ref2.Key, ".bin"), "unsafe type should fall back to .bin, got %q", ref2.Key)
}

func TestFS_New_RejectsEmptyDir(t *testing.T) {
	_, err := fs.New(fs.Config{Dir: ""})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty Dir")
}

func TestFS_New_BadCreateMode(t *testing.T) {
	_, err := fs.New(fs.Config{Dir: t.TempDir(), CreateMode: "not-octal"})
	require.Error(t, err)
}

func TestFS_FactoryWrongType(t *testing.T) {
	_, err := fs.NewFactory("not-a-config")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected fs.Config")
}

func TestFS_OpenMissingKey(t *testing.T) {
	be, err := fs.New(fs.Config{Dir: t.TempDir()})
	require.NoError(t, err)
	_, err = be.Open(context.Background(), storage.ObjectRef{Backend: "fs", Key: "nope.bin"})
	require.True(t, errors.Is(err, storage.ErrNotFound))
}
