package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hugr-lab/hugen/pkg/artifacts"
)

func wrap(prefix string, err error) error { return fmt.Errorf("%s: %w", prefix, err) }

// fakeReader is the minimal stand-in for *artifacts.Manager that the
// download handler depends on. Fields are inspected directly by the
// status-code matrix tests.
type fakeReader struct {
	infoErr   error
	infoOut   artifacts.ArtifactDetail
	openErr   error
	openBody  string
	openStat  artifacts.Stat
}

func (f *fakeReader) Info(_ context.Context, _, _ string) (artifacts.ArtifactDetail, error) {
	if f.infoErr != nil {
		return artifacts.ArtifactDetail{}, f.infoErr
	}
	return f.infoOut, nil
}

func (f *fakeReader) OpenReader(_ context.Context, _, _ string) (io.ReadCloser, artifacts.Stat, error) {
	if f.openErr != nil {
		return nil, artifacts.Stat{}, f.openErr
	}
	return io.NopCloser(strings.NewReader(f.openBody)), f.openStat, nil
}

func newDownloadServer(t *testing.T, fr *fakeReader, cfg artifactDownloadConfig) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	registerArtifactDownload(mux, fr, cfg, nil)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestArtifactDownload_StatusMatrix(t *testing.T) {
	tests := []struct {
		name       string
		fr         *fakeReader
		cfg        artifactDownloadConfig
		wantStatus int
		wantHeader map[string]string
		wantBody   string
	}{
		{
			name: "200 happy path",
			fr: &fakeReader{
				infoOut: artifacts.ArtifactDetail{
					ArtifactRef: artifacts.ArtifactRef{
						ID: "art_x", Name: "incidents", Type: "csv",
						Visibility: artifacts.VisibilityUser, SizeBytes: 5,
						StorageBackend: "fs",
					},
				},
				openBody: "a,b,c",
				openStat: artifacts.Stat{Size: 5, ContentType: "text/csv; charset=utf-8"},
			},
			wantStatus: http.StatusOK,
			wantHeader: map[string]string{
				"Content-Type":        "text/csv; charset=utf-8",
				"Content-Length":      "5",
				"Content-Disposition": `attachment; filename="incidents.csv"`,
				"Cache-Control":       "private, max-age=0",
			},
			wantBody: "a,b,c",
		},
		{
			name:       "404 unknown artifact",
			fr:         &fakeReader{infoErr: artifacts.ErrUnknownArtifact},
			wantStatus: http.StatusNotFound,
			wantBody:   "artifact not found",
		},
		{
			name:       "404 visibility miss collapses to not found",
			fr:         &fakeReader{infoErr: wrap("artifacts: Info", artifacts.ErrUnknownArtifact)},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "413 size cap",
			fr: &fakeReader{
				infoOut: artifacts.ArtifactDetail{
					ArtifactRef: artifacts.ArtifactRef{
						ID: "art_huge", Name: "big", Type: "parquet",
						Visibility: artifacts.VisibilityUser, SizeBytes: 999_999_999,
						StorageBackend: "fs",
					},
				},
			},
			cfg:        artifactDownloadConfig{MaxBytes: 1024},
			wantStatus: http.StatusRequestEntityTooLarge,
			wantBody:   "artifact size exceeds download cap",
		},
		{
			name: "503 unregistered backend",
			fr: &fakeReader{
				infoOut: artifacts.ArtifactDetail{
					ArtifactRef: artifacts.ArtifactRef{
						ID: "art_s3", Name: "stuff", Type: "csv",
						Visibility: artifacts.VisibilityUser,
						StorageBackend: "s3",
					},
				},
				openErr: artifacts.ErrUnregisteredBackend,
			},
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   `storage backend "s3" is not currently registered`,
		},
		{
			name: "500 unexpected info error",
			fr:   &fakeReader{infoErr: errors.New("boom")},
			wantStatus: http.StatusInternalServerError,
			wantBody:   "internal error",
		},
		{
			name: "500 unexpected open error",
			fr: &fakeReader{
				infoOut: artifacts.ArtifactDetail{
					ArtifactRef: artifacts.ArtifactRef{
						ID: "art_x", Name: "x", Type: "txt",
						Visibility: artifacts.VisibilityUser, StorageBackend: "fs",
					},
				},
				openErr: errors.New("disk on fire"),
			},
			wantStatus: http.StatusInternalServerError,
			wantBody:   "internal error",
		},
	}

	for _, c := range tests {
		t.Run(c.name, func(t *testing.T) {
			srv := newDownloadServer(t, c.fr, c.cfg)
			resp, err := http.Get(srv.URL + "/admin/artifacts/some_id")
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, c.wantStatus, resp.StatusCode)
			for k, want := range c.wantHeader {
				assert.Equal(t, want, resp.Header.Get(k), "header %s", k)
			}
			body, _ := io.ReadAll(resp.Body)
			if c.wantBody != "" {
				assert.Contains(t, string(body), c.wantBody)
			}
		})
	}
}

func TestArtifactDownload_DefaultsApplied(t *testing.T) {
	fr := &fakeReader{
		infoOut: artifacts.ArtifactDetail{
			ArtifactRef: artifacts.ArtifactRef{
				ID: "art_d", Name: "defaults", Type: "txt",
				Visibility: artifacts.VisibilityUser, SizeBytes: 4,
				StorageBackend: "fs",
			},
		},
		openBody: "ping",
		openStat: artifacts.Stat{Size: 4},
	}
	// Zero cfg — handler must apply WriteChunk and WriteTimeout
	// defaults rather than panic on a 0 deadline.
	srv := newDownloadServer(t, fr, artifactDownloadConfig{})
	resp, err := http.Get(srv.URL + "/admin/artifacts/art_d")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, "ping", string(body))
}

func TestArtifactDownload_ContentDispositionSanitises(t *testing.T) {
	fr := &fakeReader{
		infoOut: artifacts.ArtifactDetail{
			ArtifactRef: artifacts.ArtifactRef{
				ID: "art_d", Name: "Q1 incidents/path-with chars*", Type: "parquet",
				Visibility: artifacts.VisibilityUser, SizeBytes: 1, StorageBackend: "fs",
			},
		},
		openBody: "x",
		openStat: artifacts.Stat{Size: 1},
	}
	srv := newDownloadServer(t, fr, artifactDownloadConfig{})
	resp, err := http.Get(srv.URL + "/admin/artifacts/art_d")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	disp := resp.Header.Get("Content-Disposition")
	// All non-portable characters replaced with "_"; extension appended.
	assert.Contains(t, disp, "Q1_incidents_path-with_chars_.parquet",
		"got %q", disp)
}

func TestArtifactDownload_NilManagerNoOp(t *testing.T) {
	mux := http.NewServeMux()
	// nil Manager → no panic, no route registered.
	registerArtifactDownload(mux, (*artifacts.Manager)(nil), artifactDownloadConfig{}, nil)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/admin/artifacts/anything")
	require.NoError(t, err)
	defer resp.Body.Close()
	// No handler registered → mux 404.
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}
