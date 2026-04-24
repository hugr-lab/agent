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
        2. If the task contains a single key finding worth sharing
           with the coordinator, persist it via
           `memory_note(content: "…", scope: "parent")` so the
           coordinator (and any later specialists) inherit it on
           subsequent turns.
        3. Never ask the coordinator clarifying questions; answer
           from the information in the dispatch task.
---

# Scenario Domain Skill

This skill exists only for the scenario runner. It carries no tool
providers; the sub-agent role inherits `_memory` and `_context` from
the per-subagent autoload (spec 006 §3a).
