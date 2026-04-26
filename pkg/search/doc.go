// Package search owns multi-horizon session-context search (spec 009
// phase 4). It exposes two tools — session_context (ranked semantic
// search across turn / mission / session / user scopes with
// relevance × recency rerank) and session_events (raw structured
// inspection of one session's events).
//
// Invariant cheatsheet:
//
//   - Hugr semantic: argument. Search uses Hugr's high-level
//     semantic: { query, limit } argument on the
//     @embeddings-annotated session_events GraphQL type from phase
//     1. The embedder runs server-side; Go never calls the embedder
//     client directly from this package.
//
//   - Recency overlay in Go. The semantic: argument returns rows
//     ordered by raw cosine distance. ApplyRecency computes
//     combined = (1 − distance) × 1/(1 + age_hours/half_life) and
//     reorders. We pull limit × 3 from GraphQL for headroom so a
//     fresh-but-low-relevance match cannot drown high-relevance
//     hits.
//
//   - Scope authorization. session and user scopes are
//     coordinator-only. Sub-agents calling them get
//     ErrScopeForbidden with a hint to use ask_coordinator instead.
//     Sub-agents may call turn and mission scopes against their own
//     session.
//
//   - turn vs mission. turn is a chronological tail (no semantic,
//     no rerank, no fallback) and rejects a `query` argument.
//     mission is the same corpus but ranked.
//
//   - Keyword fallback. When the GraphQL semantic call returns zero
//     rows AND the corpus filter without `embedding IS NOT NULL`
//     would have matched non-zero rows, the tool retries the same
//     query shape with content: { ilike: "%query%" } and reports
//     strategy: "keyword". Callers do not need to know whether
//     embeddings are populated.
//
//   - Corpus resolution per scope. resolveCorpus computes the set
//     of session_ids the query runs against:
//     turn / mission → singleton caller id (no recursive walk).
//     session       → session_events_chain view rooted at the
//                     coordinator root id (recursive CTE from
//                     phase 1).
//     user          → root sessions of the same owner_id, then a
//                     batched per-root session_events_chain query
//                     (alias-multiplexed when ≤ AliasLimit, sequential
//                     with worker pool otherwise).
//
// See specs/009-hitl-search-composition/contracts/search-tools.md
// and search-corpus.md for the full contract surface.
package search
