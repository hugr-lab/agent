-- ==========================================================================
-- Migration 0.0.2 — Agent Loop Foundation (spec 006)
--
-- Additive-only. Adds:
--   • sessions.session_type discriminator (root | subagent | fork)
--   • sessions.spawned_from_event_id FK back-reference to session_events
--   • session_notes.author_session_id (separates "where visible" from "who wrote")
--   • session_events.embedding column + HNSW index (when VectorSize > 0)
-- All view/relationship adjustments live in schema.tmpl.graphql (engine
-- attaches the file at AttachRuntimeSource time; not driver SQL).
--
-- Template variables: same as schema.tmpl.sql — .VectorSize, .EmbedderModel,
-- .IsTimescale + isPostgres / isDuckDB.
-- ==========================================================================

-- ----------------------------------------------------------------------------
-- 1. sessions: discriminator + spawn-event linkage
-- ----------------------------------------------------------------------------

ALTER TABLE sessions ADD COLUMN session_type VARCHAR NOT NULL DEFAULT 'root';
ALTER TABLE sessions ADD COLUMN spawned_from_event_id VARCHAR;

{{ if isPostgres }}
CREATE INDEX IF NOT EXISTS idx_sessions_type
    ON sessions (agent_id, session_type);
{{ end }}

-- ----------------------------------------------------------------------------
-- 2. session_notes: author attribution
-- ----------------------------------------------------------------------------

ALTER TABLE session_notes ADD COLUMN author_session_id VARCHAR;

{{ if isPostgres }}
CREATE INDEX IF NOT EXISTS idx_notes_author
    ON session_notes (agent_id, author_session_id);
{{ end }}

-- ----------------------------------------------------------------------------
-- 3. session_events: semantic embedding column (only when vectors are on)
-- ----------------------------------------------------------------------------

{{ if gt .VectorSize 0 }}
ALTER TABLE session_events ADD COLUMN embedding
    {{ if isPostgres }}vector({{ .VectorSize }}){{ else }}FLOAT[{{ .VectorSize }}]{{ end }};

{{ if isPostgres }}
CREATE INDEX IF NOT EXISTS session_events_vss
    ON session_events USING hnsw (embedding vector_cosine_ops);
{{ end }}
-- See schema.tmpl.sql for the rationale on why DuckDB HNSW is intentionally
-- omitted at provision time (experimental_persistence flag + scale-driven
-- decision; mirrors the existing memory_items pattern).
{{ end }}

-- ==========================================================================
-- End of migration 0.0.2.
-- ==========================================================================
