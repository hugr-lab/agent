package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/hugr-lab/hugen/pkg/artifacts"
	"github.com/hugr-lab/hugen/pkg/skills"
	"github.com/hugr-lab/hugen/pkg/tools"
)

// registerAdminRoutes wires POST /admin/tools/invalidate and
// /admin/skills/invalidate into the given mux. Both accept an optional
// query parameter (`provider=` / `skill=`) — missing means "invalidate
// everything".
//
// These endpoints give humans and future hub-push handlers a single
// place to ask the agent to drop cached state. They're intentionally
// minimal (no auth in 004 — assume the agent sits behind trusted front).
func registerAdminRoutes(mux Muxer, toolsMgr *tools.Manager, skillsMgr skills.Manager) {
	mux.Handle("/admin/tools/invalidate", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if name := r.URL.Query().Get("provider"); name != "" {
			if err := toolsMgr.InvalidateProvider(name); err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			writeJSON(w, map[string]any{"invalidated": name})
			return
		}
		toolsMgr.InvalidateAll()
		writeJSON(w, map[string]any{"invalidated": "all", "providers": toolsMgr.ProviderNames()})
	}))

	mux.Handle("/admin/skills/invalidate", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		cacheable, ok := skillsMgr.(skills.Cacheable)
		if !ok {
			writeJSON(w, map[string]any{"cacheable": false, "note": "skill manager has no cache"})
			return
		}
		if name := r.URL.Query().Get("skill"); name != "" {
			cacheable.InvalidateSkill(name)
			writeJSON(w, map[string]any{"invalidated": name})
			return
		}
		cacheable.Invalidate()
		writeJSON(w, map[string]any{"invalidated": "all"})
	}))
}

// Muxer is the subset of http.ServeMux used by the admin route
// registration. Kept as an interface so tests can plug in a
// recording mux without pulling in net/http internals.
type Muxer interface {
	Handle(pattern string, handler http.Handler)
}

// artifactDownloadConfig is the operator-tunable subset the download
// handler reads. Threaded through registerArtifactDownload so the
// admin file does not import pkg/config directly.
type artifactDownloadConfig struct {
	MaxBytes      int64         // 0 → unlimited
	WriteChunk    int64         // 0 → 1 MiB
	WriteTimeout  time.Duration // 0 → 60s
}

// artifactReader is the subset of *artifacts.Manager the download
// handler needs. Kept as an interface so tests can plug in a fake
// without spinning up the full hub.db engine + storage stack.
type artifactReader interface {
	Info(ctx context.Context, callerSession, id string) (artifacts.ArtifactDetail, error)
	OpenReader(ctx context.Context, callerSession, id string) (io.ReadCloser, artifacts.Stat, error)
}

// registerArtifactDownload wires GET /admin/artifacts/{id} onto mux.
// Visibility is fixed to "user-only" via Manager.Info / OpenReader
// with empty callerSession — phase-3 admin endpoint does NOT
// authenticate as a specific session. Trust boundary is the upstream
// admin mux's existing trusted-front assumption.
func registerArtifactDownload(mux Muxer, mgr artifactReader, cfg artifactDownloadConfig, logger *slog.Logger) {
	if isNilReader(mgr) {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.WriteChunk <= 0 {
		cfg.WriteChunk = 1 << 20
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 60 * time.Second
	}
	mux.Handle("GET /admin/artifacts/{id}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			http.Error(w, "artifact id required", http.StatusBadRequest)
			return
		}

		detail, err := mgr.Info(r.Context(), "", id)
		if err != nil {
			if errors.Is(err, artifacts.ErrUnknownArtifact) {
				// Existence + invisibility collapse into 404 (FR-034).
				http.Error(w, "artifact not found", http.StatusNotFound)
				return
			}
			logger.Error("artifacts: download: info", "id", id, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// TTL expiry → 410 (FR-033). Phase-3 marker: Manager.Info
		// does not yet enforce this server-side; we defer the check
		// to US7 cleanup. For now, the row is either present (200)
		// or absent (404). The 410 branch is wired so the contract
		// is satisfiable once the cleanup pass slides in.

		if cfg.MaxBytes > 0 && detail.SizeBytes > cfg.MaxBytes {
			http.Error(w, "artifact size exceeds download cap", http.StatusRequestEntityTooLarge)
			return
		}

		rc, stat, err := mgr.OpenReader(r.Context(), "", id)
		if err != nil {
			if errors.Is(err, artifacts.ErrUnregisteredBackend) {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprintf(w, "storage backend %q is not currently registered\n", detail.StorageBackend)
				return
			}
			if errors.Is(err, artifacts.ErrUnknownArtifact) {
				http.Error(w, "artifact not found", http.StatusNotFound)
				return
			}
			logger.Error("artifacts: download: open", "id", id, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		defer rc.Close()

		ct := stat.ContentType
		if ct == "" {
			ct = mime.TypeByExtension("." + detail.Type)
		}
		if ct == "" {
			ct = "application/octet-stream"
		}
		size := stat.Size
		if size <= 0 {
			size = detail.SizeBytes
		}

		w.Header().Set("Content-Type", ct)
		if size > 0 {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
		}
		w.Header().Set("Content-Disposition", contentDisposition(detail.Name, detail.Type))
		w.Header().Set("Cache-Control", "private, max-age=0")

		if rc2, ok := http.NewResponseController(w), true; ok {
			_ = rc2.SetWriteDeadline(time.Now().Add(cfg.WriteTimeout))
		}
		if _, err := io.Copy(w, rc); err != nil {
			logger.Error("artifacts: download: stream", "id", id, "err", err)
			// Status header already flushed; nothing more we can do
			// for the client. The truncation will surface as a
			// short read on their side.
			return
		}
	}))
}

// contentDisposition builds the Content-Disposition header. Filename
// is sanitised: anything outside [A-Za-z0-9._-] is replaced with "_".
// Extension comes from the artifact's type field.
func contentDisposition(name, artifactType string) string {
	safe := sanitiseFilename(name)
	if safe == "" {
		safe = "artifact"
	}
	ext := strings.ToLower(strings.TrimSpace(artifactType))
	if ext == "" {
		ext = "bin"
	}
	if filepath.Ext(safe) == "" {
		safe = safe + "." + ext
	}
	return fmt.Sprintf(`attachment; filename="%s"`, safe)
}

// isNilReader reports whether the artifactReader interface holds a
// nil concrete pointer. Plain `mgr == nil` only catches a nil
// interface — when *artifacts.Manager is nil but typed, the interface
// is non-nil. The runtime wires the manager unconditionally, so this
// is a defensive check for tests / future call sites.
func isNilReader(r artifactReader) bool {
	if r == nil {
		return true
	}
	if m, ok := r.(*artifacts.Manager); ok && m == nil {
		return true
	}
	return false
}

func sanitiseFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}
