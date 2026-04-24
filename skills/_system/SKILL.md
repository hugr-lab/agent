---
name: _system
version: "0.2.0"
description: >
  Built-in agent-management tools. Always active on the user-facing
  (root) conversation. Provides skill_list / skill_load / skill_unload
  / skill_ref / skill_ref_unload for the skill catalogue, plus
  subagent_list / subagent_dispatch for delegating narrow tasks to
  specialist roles that a loaded skill declares. Context tools
  (context_status, context_intro) live in the `_context` skill;
  memory tools live in `_memory`.
autoload: true
autoload_for: [root]
providers:
  - name: _skills
    provider: _skills
  - name: _subagent
    provider: _subagent
---

# Skill and context management

These tools drive everything else. On every user message, follow this
sequence:

1. Call `skill_list` to see available skills.
2. Call `skill_load` with the most relevant skill name.
3. Look at the references returned by `skill_load`. For each reference
   whose description matches your task, call `skill_ref` before using
   any data tools. References cover syntax, field names, and query
   patterns — skipping them is the single biggest source of avoidable
   tool errors and wasted turns.
4. Only then use the skill's data tools to answer the question.

Rule of thumb for step 3: if the request involves a filter, a specific
field, or a non-trivial query shape — you almost certainly need at
least one reference loaded first. A tool error that mentions field
names, filter syntax, or operators means you should have read the
relevant reference — load it now and retry.

## Reclaiming context

Context is a scarce resource. You are allowed — and encouraged — to
unload things you no longer need:

- `skill_ref_unload` when a reference you already consumed is no longer
  useful for the remaining work. The insight you extracted stays in
  your working memory; the raw document text doesn't need to.
- `skill_unload` when the conversation moves to a different domain and
  a previously-loaded skill's tools and references are no longer
  needed. The skill stays visible in `skill_list` and can be re-loaded
  later.

Unloading is cheap and reversible. Prefer unloading over carrying
stale context into new turns.

## Budget awareness

Before loading additional references, call `context_status` (from the
`_context` skill) to check current usage. If usage is high, unload
something before loading more.

## Delegating to a sub-agent

Some skills declare specialist roles (see `subagent_list` to discover
them). A specialist runs in its own isolated session under its
configured model — often a cheaper one — so you can delegate narrow
tasks without polluting your own context with raw tool output.

When to delegate:

- A task that involves many tool calls whose intermediate results
  you don't need to see (e.g. "describe the schema of module X" →
  probably 10+ discovery/schema calls; you just want the summary).
- A task that fits a role's description precisely (a
  `schema_explorer` for schemas, a `data_analyst` for queries, etc).
- Anything you'd otherwise spend your strong-model budget on when a
  specialist-intent role can handle it for less.

When NOT to delegate:

- Questions you can answer from context or memory.
- Multi-step tasks that mix the specialist's scope with other
  domains — handle the composition yourself.
- Tasks where the user expects to see the intermediate steps.

Call `subagent_list` first to discover available roles on your
currently-loaded skills. Then `subagent_dispatch(skill, role, task,
notes?)` to run it. You get back `{summary, child_session,
turns_used, truncated, error}`. The summary is capped (≈ 800 chars by
role default) — if the specialist needed to return more, a follow-up
phase publishes artifacts; until then, a truncated summary asks for a
more focused task.
