# ADR 005 — Rolling-window context compaction

**Status**: Accepted (pre-implementation)
**Date**: 2026-04-20
**Spec**: [specs/005-memory-learning](../../specs/005-memory-learning)
**Design**: [design/006-memory-learning-skills/design.md](../../design/006-memory-learning-skills/design.md)

## Context

ADR v7.2 §7 calls for a rolling-window context manager that keeps long
conversations within the model's context budget. Without it, the agent
eventually fails a turn with a model-level `context_length_exceeded`
error — a correctness failure, not a UX nit. Constitution §VII mandates
an ADR for any change to the context-management strategy.

## Decision

Compaction runs in a `BeforeModelCallback` called on every model turn.
When accumulated context crosses a **70% ratio** of the model's budget,
the compactor summarises the oldest full *turn groups* into a single
synthetic message and extracts notable findings into
`hub.db.session_notes` so they remain in the fixed prompt part.

### Invariants

1. **Turn-group atomicity** — A `tool_call` and its matching
   `tool_result` are never split across the summary boundary. Selection
   walks the message chain and extends the compacted range until the
   next boundary sits at a valid turn-group gap.
2. **Fixed part never compacted** — Constitution, skill instructions
   (including `_memory` / `_context` / `_system`), references, memory
   status hint, and session notes are all outside the rolling window.
   Compactor only touches `req.Contents`.
3. **Observability** — Every compaction writes a `session_events` row
   with `event_type = "compaction"` and metadata
   `{from_seq, to_seq, original_turns, summary_tokens}`. The summarised
   originals remain in `session_events` (not deleted) so post-session
   review can still read them.
4. **Intent-based routing** — Summarisation is reached through
   `model.Options{Intent: Summarization}`, never a second `model.LLM`
   instance. The router picks the cheap model.

### Trigger ratio

Default `0.70`. Configurable via `memory.compaction_threshold`.
Rationale:

- Below 0.70 there is enough slack for a worst-case next turn (large
  tool result + a few retries) without triggering a second compaction
  in quick succession.
- Above 0.80 leaves no headroom — a single large tool result can still
  overflow.
- Too aggressive (< 0.50) means compacting fresh, still-relevant
  history. The 70% threshold is the ADR default; revisit only on
  measured regressions.

### Target-tokens per run

Compactor reclaims ~2048 tokens per invocation. Enough to avoid a
same-turn re-trigger; small enough that a single compaction is cheap.

### Session-note extraction

The summarisation prompt asks the cheap model to emit both a prose
summary (stays in `req.Contents`) **and** a short JSON list of notable
findings (each ≤ 150 chars). Each finding lands in `session_notes`
via `hub.Sessions.AddNote` so it survives subsequent compactions — the
note block is part of the fixed prompt.

## Alternatives considered

| Option | Why rejected |
|---|---|
| Keep everything in context, rely on model's built-in long-context capabilities | Token cost grows unbounded; response latency inflates; some models degrade past ~70% of their stated window even before the hard cut-off. |
| Drop oldest turns with no summary | Loses information the agent already paid tokens to obtain; next turn re-discovers. |
| Per-message summarisation (continuous) | LLM call on every turn; expensive; noisy since most turns don't need it. |
| Fixed-size sliding window without LLM summary | Same loss-of-information problem; agent can't reason about compacted content at all. |
| Separate compactor `model.LLM` instance | Violates "agent knows only data-source names" (constitution §II); duplicates auth plumbing. Rejected in favour of intent routing. |

## Consequences

**Positive:**

- Long sessions never hit `context_length_exceeded`.
- Session notes extracted during compaction persist beyond the compacted
  turns — facts carry forward.
- All summarised content is still in `session_events` for post-session
  review, so review quality is unaffected by compaction.

**Negative / caveats:**

- Summary quality depends on the cheap model. Operators who route
  `summarization` to a too-small model may see noise in summaries.
- First-trigger latency is one cheap-model call (~1-2 s typical).
  Amortised across hundreds of turns, this is negligible.
- Turn-group atomicity occasionally leaves the actual compaction boundary
  a few turns later than 70% — accepted to preserve correctness.

## Operational notes

- Knob: `memory.compaction_threshold` (default 0.70).
- Metric: count `session_events.event_type = "compaction"` per session
  to spot pathological frequency.
- Disable for tests by omitting the compactor callback from the
  runtime's `BeforeModelCallbacks` chain.
