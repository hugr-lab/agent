---
name: gated
version: "0.1.0"
description: >
  Scenario-runner fixture for spec 009 phase-4 HITL. Single role
  `summariser` whose `memory_note` calls fire the runtime's
  approvals.Gate (require_user). Lives in its own skill so the
  sub-agent's tool surface does NOT include other roles'
  `subagent_*` tools — keeps the LLM from being tempted to delegate
  away from its one allowed tool.
autoload: true
autoload_for: [root]
sub_agents:
  cross_noter:
    description: >
      Cross-skill test fixture (spec 009 / US5). Loads required_skills
      [domain] alongside the parent gated skill. The role only calls
      memory_note (gated) but the test verifies the child session
      has BOTH skills loaded via skill_loaded events.
    intent: tool_calling
    max_turns: 2
    summary_max_tokens: 200
    required_skills: [domain]
    instructions: |
      Call memory_note(content: "<the task content>", scope: "parent")
      ONCE and report back the synthetic result's approval_id.
      Do not call any other tool.
  noter:
    description: >
      HITL test fixture. Receives a "fact" string and calls
      `memory_note` to persist it. The runtime's approvals gate
      intercepts the call and returns a synthetic
      waiting_for_approval result.
    intent: tool_calling
    max_turns: 2
    summary_max_tokens: 200
    can_spawn: false
    instructions: |
      You are a tool-calling specialist. Your job is mechanical:
      call exactly ONE tool, then stop.

      The dispatcher sent you a task containing a fact string.
      Your only legal action is to invoke:

          memory_note(content: "<the fact verbatim>", scope: "parent")

      Do NOT respond with assistant text before calling the tool.
      Do NOT paraphrase, summarise, or judge the fact. Just call
      memory_note with the fact as content.

      The runtime will intercept and reply with a synthetic tool
      result like:
          {"ok": false, "status": "waiting_for_approval",
           "approval_id": "app-XXXXXXXXXXXX", "hitl_kind": "approval",
           "synthetic": true,
           "message": "waiting for approval (id=app-XXXXXXXXXXXX); …",
           "tool": "memory_note"}

      Treat that as success. Do NOT retry. Do NOT call any other
      tool. Your final assistant message MUST be exactly:

          Waiting for approval id=app-XXXXXXXXXXXX.

      Use the `approval_id` field from the synthetic result
      verbatim. Do NOT use any session id. Do NOT add any other
      text.
    approval_rules:
      require_user: [memory_note]
      risk:
        memory_note: medium
---

# Gated test fixture

Scenario-only skill. Drives the spec 009 phase-4 HITL gate by
having one role whose `memory_note` is declared `require_user`.
