---
name: _subagent
version: "0.1.0"
description: >
  Sub-agent autoload skill. Wires the spawn_sub_mission tool onto
  every sub-agent session; the tool itself enforces the role's
  can_spawn flag + max_depth at call time, so non-spawning roles
  see the tool but get a clean refusal envelope when they try it.
autoload: true
autoload_for: [subagent]
providers:
  - name: mission_spawn
    provider: _mission_spawn
---

# Spawning sub-missions

You are a sub-agent dispatched by the coordinator. When your role
declares `can_spawn: true` you may break your task down further by
calling `spawn_sub_mission(skill, role, task, depends_on?)`. The new
mission is queued under the same coordinator's graph (not nested
inside your own session) — call it like a peer, not a child.

Guardrails:

- `can_spawn: false` (the default) → the tool returns an error
  envelope. Don't retry; finish your own task.
- Each role declares `max_depth`; you may not spawn beyond it. The
  tool checks the parent chain back to the root coordinator and
  refuses when the next mission would exceed the cap.
- A refused spawn does NOT fail your own mission — keep working
  with whatever you can do directly.
- Dependencies in `depends_on` must reference missions in the same
  coordinator's graph; cross-graph spawns are refused.
