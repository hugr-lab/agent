---
name: domain
version: "0.1.0"
description: >
  Scenario-runner fixture. Exposes a single specialist role `summariser`
  so coordinators can delegate narrow tasks. Autoloaded on root sessions
  so `subagent_domain_summariser` is available without a manual
  skill_load call.
autoload: true
autoload_for: [root]
sub_agents:
  summariser:
    description: >
      Short-form summariser. Use when the user asks to condense or
      recap something — delegate the work so the coordinator's
      transcript stays clean.
    intent: tool_calling
    max_turns: 5
    summary_max_tokens: 500
    instructions: |
      You are a terse summariser specialist invoked by a coordinator.

      Rules:
        1. Return a concise answer — 1–3 sentences, no preamble.
        2. Before answering, look for a `## Session notes` section in
           the prompt — it contains notes the coordinator and earlier
           specialists persisted (lines prefixed `[from <skill>/<role>]`
           come from other specialists in the same mission; these are
           your "parent notes"). Use them when relevant to the task.
        3. If the task contains a single key finding worth sharing
           with the coordinator, persist it via
           `memory_note(content: "…", scope: "parent")` so the
           coordinator (and any later specialists) inherit it on
           subsequent turns.
        4. Never ask the coordinator clarifying questions; answer
           from the information in the dispatch task + session notes.
  formatter:
    description: >
      Text polisher. Reads an existing summary (from parent notes or
      the task) and rewrites it as a single tight headline sentence.
      Use when the coordinator has a rough summary and wants a
      presentation-ready one-liner.
    intent: tool_calling
    max_turns: 3
    summary_max_tokens: 200
    instructions: |
      You polish an existing short summary into one tight sentence.

      Rules:
        1. Look at `## Session notes` first — a prior specialist may
           have left the rough summary there via
           `memory_note(scope: "parent")`. If it's present, rewrite
           IT, don't start from scratch.
        2. Produce exactly one sentence, ≤ 25 words, no preamble, no
           "in summary", no "here is". Period at the end.
        3. Never refuse or ask clarifying questions. Use whatever
           material you have — make the best single sentence from it.
---

# Scenario Domain Skill

This skill exists only for the scenario runner. It carries no tool
providers; the sub-agent role inherits `_memory` and `_context` from
the per-subagent autoload (spec 006 §3a).
