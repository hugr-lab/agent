package mcp

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew_RequiresEndpointForHTTP(t *testing.T) {
	_, err := New("empty", Options{TransportType: TransportStreamableHTTP})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "streamable-http needs endpoint")

	_, err = New("empty", Options{TransportType: TransportSSE})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sse needs endpoint")
}

func TestNew_RequiresCommandForStdio(t *testing.T) {
	_, err := New("empty", Options{TransportType: TransportStdio})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stdio needs command")
}

func TestNew_UnknownTransport(t *testing.T) {
	_, err := New("empty", Options{TransportType: Transport("mystery")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported transport")
}

func TestNew_DefaultsToStreamableHTTP(t *testing.T) {
	// TransportType empty → defaults to streamable-http → requires
	// endpoint, so this exercises the default-resolution branch.
	_, err := New("empty", Options{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "streamable-http needs endpoint")
}

func TestNew_StreamableHTTP_OK(t *testing.T) {
	p, err := New("s", Options{
		TransportType: TransportStreamableHTTP,
		Endpoint:      "http://localhost:0/mcp",
	})
	require.NoError(t, err)
	assert.Equal(t, "s", p.Name())
	assert.Equal(t, TransportStreamableHTTP, p.TransportType())
	assert.Equal(t, "http://localhost:0/mcp", p.Endpoint())
}

func TestInvalidate_ClearsFetchedTimestamp(t *testing.T) {
	p, err := New("s", Options{
		TransportType: TransportStreamableHTTP,
		Endpoint:      "http://localhost:0/mcp",
	})
	require.NoError(t, err)

	// Direct state poke: after a synthesised "fetch happened" Invalidate
	// must reset `fetched` so the next Tools() call treats the cache as
	// expired.
	p.mu.Lock()
	p.fetched = time.Now()
	p.mu.Unlock()

	p.Invalidate()

	p.mu.Lock()
	fetched := p.fetched
	p.mu.Unlock()
	assert.True(t, fetched.IsZero(), "Invalidate must zero fetched")
}
