// Package learning owns the agent's memory/learning runtime pieces that
// are NOT part of the storage adapter: per-skill memory configuration
// parsing + merge, post-session review, hypothesis verification,
// periodic consolidation, rolling-window context compaction, and the
// per-turn "Memory Status" instruction-provider decorator.
//
// The adapter surface (reads/writes against hub.db) lives in
// adapters/hubdb/. The system tools the LLM calls
// (memory_search / memory_note / context_compress / ...) live in
// pkg/tools/system. This package sits between those two layers: it
// consumes HubDB + model.LLM and produces the policies + callbacks
// that drive the learning loop.
//
// Nothing in this package depends on pkg/session — the session
// manager publishes work to pkg/scheduler, which in turn calls
// functions from this package. Keeping the direction one-way avoids
// import cycles and keeps background work independent of any single
// conversation's lifetime.
package memory
