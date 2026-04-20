---
name: _memory
description: >
  Persistent memory tools. Save notes during the session, search
  long-term facts learned from previous sessions.
autoload: true
providers:
  - provider: _memory
---

# Memory

You have two kinds of memory.

## Session notes (this conversation only)

- `memory_note(content)` — save a concise finding to the scratchpad.
  Notes stay visible in your prompt until the session ends.
- `memory_clear_note(id)` — remove a note when it's no longer useful.

## Long-term memory (learned from previous sessions)

- `memory_search(query, tags?, category?)` — find facts. Each fact
  shows `age_days` and `expires_in_days` — use these to judge
  freshness.
- `memory_linked(id, depth=1)` — navigate from one fact to related
  facts (schema → query templates → anti-patterns).
- `memory_stats()` — what's in memory by category.

## Rule of thumb

BEFORE exploring data, `memory_search` first. If you find a recent
fact with `stable` volatility, trust it. Save notes freely — they
survive context compaction.

You cannot write to long-term memory directly; post-session review
extracts facts from your transcript + notes.
