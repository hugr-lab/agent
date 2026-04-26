// Package artifacts is the persistent artifact registry: producers
// (sub-agents and the coordinator) publish bulky outputs as
// references; consumers list, search, query, and download those
// references through a small tool surface. The bytes never re-enter
// an LLM context.
//
// Layout:
//
//	pkg/artifacts/
//	  manager.go        // *Manager — implements tools.Provider AND adk artifact.Service
//	  publish.go        // Publish / Remove / WidenVisibility / AddGrant
//	  read.go           // ListVisible / Info / Chain / LocalPathFor / OpenReader
//	  visibility.go     // session_artifacts view query helpers
//	  cleanup.go        // Manager.Cleanup body for scheduler cron
//	  refs.go           // ArtifactRef / Visibility / TTL / Stat / etc.
//	  tools.go          // unexported tool structs: artifact_publish ... artifact_chain
//	  store/            // DB layer — analogous to pkg/sessions/store
//	  storage/          // bytes layer — pluggable backends (fs, s3 stub)
//
// Invariants:
//
//   - *Manager is the only exported type; it implements tools.Provider
//     (Name/Tools) and ADK artifact.Service (Save/Load/Delete/List)
//     directly, no adapter struct.
//   - Bytes never enter session_events. Manager NEVER reads
//     backend-specific config keys (cfg.Artifacts.FS.Dir etc.); all
//     bytes flow through Storage.{Put,Open,LocalPath}.
//   - Visibility is resolved exclusively through the GraphQL
//     session_artifacts view; the manager NEVER joins artifacts to
//     artifact_grants from Go.
//   - WidenVisibility may only widen scope — narrowing returns
//     ErrVisibilityNarrowing.
//   - Schema indexes for artifacts / artifact_grants are
//     Postgres-only; DuckDB stays index-free.
package artifacts
