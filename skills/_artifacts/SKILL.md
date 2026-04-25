---
name: _artifacts
version: "0.6.0"
description: >
  Persistent artifact registry. Publish bulky outputs (Parquet, CSV,
  HTML, charts, generated reports) as references that the
  coordinator can render as downloadable links and that other
  missions can query without inlining the bytes into the LLM
  context.
autoload: true
autoload_for: [root, subagent]
providers:
  - name: _artifacts
    provider: _artifacts
---

# Artifacts

The artifact registry holds files produced by sessions. Bytes live
outside the conversation: in the configured storage backend on
disk (or, in future, an object store). What enters your context is
a **reference** — an opaque artifact id plus its name, type, and a
short description.

## When to publish

Publish whenever your output is bulkier than a paragraph. Concrete
triggers:

- A tabular result with more than ~20 rows.
- An HTML/markdown report you wrote to a temp file.
- A generated chart (SVG / PNG) or PDF.
- Any binary blob a downstream consumer might re-read.

Do NOT publish when a one-line summary covers it — the existing
session-event transcript already carries short text outputs.

## How to publish

Call `artifact_publish(name, type, description, ...)` with one of:

- `path` — an absolute filesystem path you (or your tools) wrote
  the bytes to. Best for any file ≥ 1 MiB.
- `inline_bytes` — base64-encoded payload (capped at 1 MiB by
  default). Best for small in-memory blobs.

Required fields: `name` (display name, no extension), `type`
(`parquet | csv | json | html | svg | pdf | txt | md | bin`),
`description` (one or two sentences — this is what semantic search
ranks against).

Optional fields:

- `visibility` — who can see the artifact. Defaults to `self`
  (only your session). Set `parent` so the coordinator can see it;
  `graph` so every mission in the same coordinator graph can see
  it; `user` so it shows up on the user's download surface. The
  coordinator can widen later but cannot narrow.
- `tags` — free-form filters used by `artifact_list`.
- `derived_from` — id of the artifact this one was produced from
  (lineage chain).
- `ttl` — `session` (default) | `7d` | `30d` | `permanent`.
  Cleanup removes expired artifacts during the daily cron.

The call returns a JSON envelope with the new artifact id. Cite
that id in your mission summary so the coordinator can render it.

## Worked example

You finished a Parquet pull and wrote it to `/tmp/q1-incidents.parquet`.

```text
artifact_publish(
  path: "/tmp/q1-incidents.parquet",
  name: "Q1 incidents (BW region)",
  type: "parquet",
  description: "Q1 2026 incident table for the BW region: 278 rows × 11 columns including severity, station_id, opened_at, closed_at.",
  visibility: "parent",
  tags: ["incidents", "Q1", "BW"]
)
```

Returns `{"id": "art_ag01_…_…", "name": "Q1 incidents (BW region)", …}`.
End your mission summary with: *"Published as artifact `art_ag01_…_…` (Q1 incidents)."*

The coordinator's user-facing reply will then surface it as
`[Q1 incidents (BW region)](artifact:art_ag01_…_…)` — a
markdown link the user can click to download.

## Error envelopes

Tool failures come back as `{"error": "...", "code": "..."}` on
the same call shape — they do NOT abort your mission. Common
codes:

- `description_required` — empty / whitespace description.
- `source_ambiguous` — neither / both of `path` and `inline_bytes`
  set; pick exactly one.
- `inline_bytes_too_large` — exceeds the 1 MiB cap; write to a
  file and pass `path` instead.
- `invalid_visibility` / `invalid_ttl` — typo in the enum field.
- `backend_not_implemented` — operator selected the s3 stub; ask
  the user to switch back to the fs backend.
- `internal` — anything else; the message carries the underlying
  reason.

## Inspecting metadata

`artifact_info(id)` returns the registered metadata for an artifact
you can see: `name`, `type`, `size_bytes`, `description`, `tags`,
`storage_backend`, `created_at`, plus tabular fields (`row_count`,
`col_count`, `file_schema`) when available. Use it before
`artifact_query` to confirm the artifact still exists and to learn
its schema. Visibility miss returns `{error, code: "unknown_artifact"}`
— the call cannot leak the existence of artifacts you do not own.

## Rendering artifacts in user replies

When you finish a coordinator turn that produced user-visible
artifacts, render each one as a markdown download link:

```markdown
[Q1 incidents (BW region)](artifact:art_ag01_…_…)
```

The link target uses the literal `artifact:` URI scheme — the
front-end resolver swaps it for the `/admin/artifacts/{id}`
download URL at render time. **Only render artifacts whose
visibility is `user`** (or that the coordinator has explicitly
widened). Self / parent / graph scoped artifacts are private to
the mission graph and must NOT appear in the user-facing reply.

## Listing visible artifacts

`artifact_list(type?, tags?, search?, limit?)` returns every artifact
your session can see (own, parent-scope from your sub-agents,
graph-scope from siblings, explicit grants, and `user`-scope
artifacts published by anyone). Default limit 50, max 200.
Each entry carries `id`, `name`, `type`, `visibility`, `size_bytes`,
`tags`. Use this before `artifact_info` to discover what's
available; cite the ids in your reasoning.

When `search` is set (≥ 3 chars and the embedder is online), the
results are ranked by description similarity — each entry carries
`distance_to_query` (lower = more similar). Combine `search` with
`type` / `tags` to scope the candidate set; type/tag filters apply
*after* semantic ranking so the top hits are always the most
relevant within the constraint.

## Visibility (coordinator only)

`artifact_visibility(id, visibility?, target_session_id?,
target_agent_id?)` is **coordinator-only**. Two operations, either
or both at once:

- **Widen scope**: `visibility: "user"` lifts an artifact to
  user-visible so it shows up on the download surface. Strict
  widening — you cannot narrow (`{error, code: visibility_narrowing}`).
- **Grant access**: `target_session_id: "sess-X"` adds an explicit
  row in `artifact_grants` so that single session can see the
  artifact regardless of its scope. Used by mission-input handoff.

Sub-agents calling this get `{error, code: not_coordinator}` —
escalate to the coordinator if you need a wider scope or a grant.

## Removing artifacts

`artifact_remove(id)` deletes an artifact (bytes + metadata + all
grants). You may remove your own artifacts; coordinators may also
remove `user`-visibility artifacts. Anything else returns
`{error, code: not_authorised}`. Idempotent on re-call (subsequent
calls return `unknown_artifact`).

## Lineage walks (coordinator only)

`artifact_chain(id)` returns the `derived_from` lineage of an
artifact, oldest first → leaf last. Each entry carries `id`,
`name`, `type`, `visibility`. Invisible ancestors appear as
`{name: "<hidden>"}` placeholders so the coordinator sees the
chain depth without leaking metadata of inaccessible nodes. The
walk stops at the first hidden ancestor (we don't know what it
was derived from without reading the row).

Sub-agents calling this get `{error, code: not_coordinator}` —
escalate to the coordinator if you need a lineage trace.

## Surface still pending

`artifact_query` (analytical SQL across artifacts) lands when the
local DuckDB sandbox MCP server arrives. Phase-3 ships publish,
info, list, visibility, remove, and chain — the artifact
lifecycle is fully closed.
