package s3_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hugr-lab/hugen/pkg/artifacts/storage"
	"github.com/hugr-lab/hugen/pkg/artifacts/storage/s3"
)

func TestS3_New_ValidatesConfig(t *testing.T) {
	_, err := s3.New(s3.Config{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Bucket")

	_, err = s3.New(s3.Config{Bucket: "b"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Region")

	be, err := s3.New(s3.Config{Bucket: "b", Region: "eu-central-1"})
	require.NoError(t, err)
	assert.Equal(t, "s3", be.Name())
}

func TestS3_AllOpsReturnNotImplemented(t *testing.T) {
	be, err := s3.New(s3.Config{Bucket: "b", Region: "eu-central-1"})
	require.NoError(t, err)

	ctx := context.Background()
	ref := storage.ObjectRef{Backend: "s3", Key: "x"}

	_, err = be.Put(ctx, storage.PutHint{ID: "x"}, strings.NewReader("y"))
	require.True(t, errors.Is(err, storage.ErrNotImplemented), "Put: got %v", err)

	_, err = be.Open(ctx, ref)
	require.True(t, errors.Is(err, storage.ErrNotImplemented), "Open: got %v", err)

	_, err = be.Stat(ctx, ref)
	require.True(t, errors.Is(err, storage.ErrNotImplemented), "Stat: got %v", err)

	require.True(t, errors.Is(be.Delete(ctx, ref), storage.ErrNotImplemented), "Delete")
}

func TestS3_LocalPath_ReturnsNotOk(t *testing.T) {
	be, err := s3.New(s3.Config{Bucket: "b", Region: "eu-central-1"})
	require.NoError(t, err)

	path, ok, err := be.LocalPath(context.Background(), storage.ObjectRef{Backend: "s3", Key: "x"})
	require.NoError(t, err) // legitimate "no local path", not an error.
	assert.False(t, ok)
	assert.Empty(t, path)
}

func TestS3_FactoryWrongType(t *testing.T) {
	_, err := s3.NewFactory("not-a-config")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected s3.Config")
}
