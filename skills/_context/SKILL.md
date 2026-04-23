---
name: _context
version: "0.1.0"
description: >
  Tools for managing your own conversation context: check current
  usage, see what's loaded, and trigger compaction when needed.
autoload: true
providers:
  - name: _context
    provider: _context
---

# Context management

Your conversation context has a finite budget. The system compacts
the oldest turns automatically once a threshold is crossed, but you
can — and should — manage context yourself to stay efficient.

- `context_status()` — current token usage: prompt size, loaded tool
  count, rough percentage of the budget.
- `context_intro()` — short summary of what's currently in your
  context (skills loaded, notes saved, memory hint counts).
- `context_compress()` — ask the system to compress the oldest turns
  now instead of waiting for the automatic threshold. Useful when
  you know old context is no longer relevant for the task at hand.

Pair with `skill_unload` / `skill_ref_unload` from the `_system`
skill when you're done with a domain — those free fixed-part tokens
that compaction cannot touch.
