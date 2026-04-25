package storage

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeBackend is a no-op Storage used to exercise the registry. Real
// behaviour-level tests live in storage/fs and storage/s3.
type fakeBackend struct{ name string }

func (f *fakeBackend) Name() string { return f.name }
func (f *fakeBackend) Put(_ context.Context, _ PutHint, _ io.Reader) (ObjectRef, error) {
	return ObjectRef{Backend: f.name, Key: "k"}, nil
}
func (f *fakeBackend) Open(_ context.Context, _ ObjectRef) (io.ReadCloser, error) {
	return nil, errors.New("not used in this test")
}
func (f *fakeBackend) Stat(_ context.Context, _ ObjectRef) (Stat, error) { return Stat{}, nil }
func (f *fakeBackend) Delete(_ context.Context, _ ObjectRef) error       { return nil }
func (f *fakeBackend) LocalPath(_ context.Context, _ ObjectRef) (string, bool, error) {
	return "", false, nil
}

func newFactory(name string) Factory {
	return func(cfg any) (Storage, error) { return &fakeBackend{name: name}, nil }
}

func TestRegistry_RoundTrip(t *testing.T) {
	t.Cleanup(reset)
	reset()
	Register("fake", newFactory("fake"))

	s, err := Open("fake", nil)
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.Equal(t, "fake", s.Name())
}

func TestRegistry_UnknownBackend(t *testing.T) {
	t.Cleanup(reset)
	reset()

	_, err := Open("nope", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown backend")
	assert.Contains(t, err.Error(), `"nope"`)
}

func TestRegistry_DuplicateRegister_Panics(t *testing.T) {
	t.Cleanup(reset)
	reset()

	Register("dup", newFactory("dup"))
	assert.Panics(t, func() {
		Register("dup", newFactory("dup"))
	})
}

func TestRegistry_NilFactory_Panics(t *testing.T) {
	t.Cleanup(reset)
	reset()
	assert.Panics(t, func() { Register("x", nil) })
}

func TestRegistry_EmptyName_Panics(t *testing.T) {
	t.Cleanup(reset)
	reset()
	assert.Panics(t, func() { Register("", newFactory("x")) })
}

func TestRegistry_FactoryError(t *testing.T) {
	t.Cleanup(reset)
	reset()
	Register("boom", func(any) (Storage, error) { return nil, errors.New("kaboom") })
	_, err := Open("boom", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kaboom")
}

func TestSentinelErrorsAreDistinct(t *testing.T) {
	assert.NotEqual(t, ErrNotFound, ErrNotImplemented)
	assert.True(t, errors.Is(ErrNotFound, ErrNotFound))
	assert.False(t, errors.Is(ErrNotFound, ErrNotImplemented))
}
