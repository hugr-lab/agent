---
name: _memory
version: "0.1.0"
description: >
  Persistent memory tools. Save notes during the session, search
  long-term facts learned from previous sessions.
autoload: true
providers:
  - name: _memory
    provider: _memory
---

# Memory

You have two kinds of memory.

## Session notes (this conversation only)

- `memory_note(content)` — save a concise finding to the scratchpad.
  Notes stay visible in your prompt until the session ends and
  survive context compaction.
- `memory_clear_note(id)` — remove a note when it's no longer useful.

## Long-term memory (learned from previous sessions)

- `memory_search(query, tags?, category?)` — find facts. Each fact
  shows `age_days` and `expires_in_days` — use those to judge
  freshness.
- `memory_linked(id, depth=1)` — walk outgoing links from one fact
  (e.g. schema → query templates → anti-patterns).
- `memory_stats()` — totals per category currently in memory.

Memory categories are **skill-scoped** — they are stored in the DB as
`<skill>.<name>` (e.g. `hugr-data.schema`, `_memory.user_preferences`)
so two skills can use the same short name without collision. The
catalog ("## Available Skills") lists the fully-qualified names for
every skill, and each active skill repeats its own list in a
"### Memory categories" block. Pass that fully-qualified form as the
`category` argument to `memory_search`, and aim for one of those
categories when saving a note you want the post-session reviewer to
promote into long-term memory.

## When to use memory (firm habits, not ritual)

- **At the start of any new topic** — not only at session open.
  When the user's question shifts focus, run `memory_search` with
  the new topic's keywords before exploring data or loading refs.
  A recent fact with `stable` volatility is trustworthy and saves
  a full exploration round.
- **Right after you learn something durable** — a correction, an
  error→fix pair, a confirmed schema detail, a user preference —
  call `memory_note` immediately, while the context is still
  precise. Don't batch notes to the end of the turn.
- **When `## Memory Status` shows pending reviews or a high fact
  count for the topic at hand** — start with `memory_search` rather
  than fresh exploration; you are very likely to find prior work.
- **When `context_status` shows high usage** (see `_context` skill)
  — write key findings as notes so they persist into the compacted
  prompt, then free space with `skill_ref_unload`.

You cannot write to long-term memory directly. A post-session
reviewer extracts facts from your transcript + notes and assigns
them to categories; well-shaped notes with a clear category and
tag hints get promoted more reliably.
