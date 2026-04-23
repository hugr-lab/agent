---
name: _system
version: "0.1.0"
description: >
  Built-in skill-management tools. Always active. Provides
  skill_list, skill_load, skill_unload, skill_ref, skill_ref_unload.
  Context tools (context_status, context_intro, context_compress)
  live in the `_context` skill.
autoload: true
providers:
  - name: _skills
    provider: _skills
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
