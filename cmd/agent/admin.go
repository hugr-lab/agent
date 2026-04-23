package main

import (
	"encoding/json"
	"net/http"

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

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}
