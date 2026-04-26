---
name: _search
version: "0.1.0"
description: >
  Multi-horizon session-context search. Tools to retrieve events
  ranked by relevance × recency at four scopes — your last few
  events (turn), your own session (mission), the entire current
  conversation tree (session, coord-only), or every prior root
  session for the same user (user, coord-only). Use when the user
  references something you said earlier, or when looking for
  cross-conversation history.
autoload: true
autoload_for: [root, subagent]
providers:
  - name: _search
    provider: _search
---

# Session search

This skill exposes two read-only tools you can use to retrieve
events from the agent's transcript history. They complement
`memory_search` (long-term promoted facts) — search reaches into
raw transcript material that may not be promoted yet.

## When to use

- The user references something **you said earlier in this
  conversation** ("what was the second option you mentioned?",
  "remind me of the cleanup criteria") → `session_context(query,
  scope: "session")`.
- The user references **a past conversation** ("what did we decide
  about severity mapping last week?") → `session_context(query,
  scope: "user", date_from?, date_to?)`.
- You want to **review your own last few events** (debugging your
  own behavior, recovering from a long pause) →
  `session_context(scope: "turn", last_n: 10)`.
- A specialist needs to **re-read its own mission's transcript**
  (e.g. it received a new instruction and wants to recheck what
  it had already decided) → `session_context(query, scope:
  "mission")`.

## Tools

- **`session_context(scope, query?, last_n?, half_life?, date_from?,
  date_to?)`**
  - `scope` (required): `turn` | `mission` | `session` | `user`.
  - `query` (optional, semantic): omit for recency-only ordering;
    rejected for `scope=turn`.
  - `last_n`: max results (default 20, hard cap 200).
  - `half_life`: tuning knob for recency decay. Default scales with
    scope (`mission=1h`, `session=24h`, `user=168h`). Pass shorter
    half-lives when looking for recent material; longer when the
    user's question is historical.
  - `date_from` / `date_to`: RFC3339 timestamps to clamp the
    corpus.
  - Returns: `{hits: [...], strategy: semantic|keyword|recency,
    scope, half_life, total_corpus_size, count}`. Each hit carries
    `session_id`, `created_at`, and `content_excerpt` so you can
    quote with provenance.

- **`session_events(session_id, tool_name?, author?, event_type?,
  limit?, order?)`**
  - Audit/debug: raw events from one specific session. No ranking,
    no semantic. Use when you need exact transcript order.

## Scope authorization

- `turn` and `mission` — open to coord and sub-agents.
- `session` and `user` — coordinator-only. Sub-agents calling
  these get `code=scope_forbidden` with a hint to use
  `ask_coordinator` instead.

## Citation discipline

When you quote a search result back to the user, include enough
provenance for them to find it:

- For `scope=session` / `mission`: cite the event's session_id and
  date. "On 2026-04-19 (sub_xxx) the analyst noted that …".
- For `scope=user`: include the date prominently — "Last week we
  decided that …".
- When `strategy=keyword` (no embeddings hit), say so honestly:
  "I found this via substring match — it might not be the
  most relevant".

## Empty results

If `count=0`, say so plainly. Don't fabricate context. Offer to
widen the date window, change scope, or rephrase the query.
