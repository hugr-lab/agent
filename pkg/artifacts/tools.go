package artifacts

import (
	"encoding/base64"
	"errors"
	"fmt"

	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"

	"github.com/hugr-lab/hugen/pkg/tools"
)

// All tool struct types in this file are unexported. They hold a
// *Manager back-reference and dispatch to the manager's domain
// methods. The manager itself is the tools.Provider — no separate
// adapter layer.
//
// US1 ships artifactPublishTool. Other tools register here as their
// owning stories land:
//
//	US2 — artifactInfoTool
//	US3 — artifactQueryTool
//	US4 — artifactRemoveTool, artifactVisibilityTool, artifactListTool
//	US9 — artifactChainTool

// ─────────────────────────────────────────────────────────────────
// artifact_publish
// ─────────────────────────────────────────────────────────────────

type artifactPublishTool struct {
	m *Manager
}

func (t *artifactPublishTool) Name() string { return "artifact_publish" }

func (t *artifactPublishTool) Description() string {
	return "Publishes a file or in-memory bytes as an artifact. Returns an opaque artifact id you can cite in your summary so the coordinator can render it as a downloadable link without inlining the bytes. Use 'path' for files ≥ 1 MiB; use 'inline_bytes' (base64) for small in-memory payloads."
}

func (t *artifactPublishTool) IsLongRunning() bool { return false }

func (t *artifactPublishTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: "OBJECT",
			Properties: map[string]*genai.Schema{
				"path": {
					Type:        "STRING",
					Description: "Absolute filesystem path the producer wrote. Mutually exclusive with inline_bytes.",
				},
				"inline_bytes": {
					Type:        "STRING",
					Description: "Base64-encoded bytes. Capped by InlineBytesMax (1 MiB by default). Mutually exclusive with path.",
				},
				"name": {
					Type:        "STRING",
					Description: "Display name (no extension; use the type field for that).",
				},
				"type": {
					Type:        "STRING",
					Description: "Artifact type: parquet | csv | json | html | svg | pdf | txt | md | bin.",
				},
				"description": {
					Type:        "STRING",
					Description: "One- to two-sentence description. Indexed into the description embedding for semantic search.",
				},
				"visibility": {
					Type:        "STRING",
					Description: "Who can see the artifact: self (default) | parent | graph | user. Coordinator can widen later.",
					Enum:        []string{"self", "parent", "graph", "user"},
				},
				"tags": {
					Type:        "ARRAY",
					Items:       &genai.Schema{Type: "STRING"},
					Description: "Optional tags for filtering in artifact_list.",
				},
				"derived_from": {
					Type:        "STRING",
					Description: "Optional parent artifact id for lineage tracking.",
				},
				"ttl": {
					Type:        "STRING",
					Description: "TTL classification: session (default) | 7d | 30d | permanent.",
					Enum:        []string{"session", "7d", "30d", "permanent"},
				},
			},
			Required: []string{"name", "type", "description"},
		},
	}
}

func (t *artifactPublishTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *artifactPublishTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	m, ok := args.(map[string]any)
	if !ok {
		return errEnvelope("artifact_publish", fmt.Errorf("unexpected args type %T", args), "invalid_args")
	}

	pubReq := PublishRequest{
		CallerSessionID: ctx.SessionID(),
		Name:            stringArg(m, "name"),
		Type:            stringArg(m, "type"),
		Description:     stringArg(m, "description"),
		DerivedFrom:     stringArg(m, "derived_from"),
		Tags:            stringSliceArg(m, "tags"),
	}
	if v := stringArg(m, "visibility"); v != "" {
		pubReq.Visibility = Visibility(v)
	}
	if v := stringArg(m, "ttl"); v != "" {
		pubReq.TTL = TTL(v)
	}

	pathArg := stringArg(m, "path")
	inlineArg := stringArg(m, "inline_bytes")
	switch {
	case pathArg != "" && inlineArg != "":
		return errEnvelope("artifact_publish", ErrSourceAmbiguous, "source_ambiguous")
	case pathArg != "":
		pubReq.Source = PublishSource{Path: pathArg}
	case inlineArg != "":
		raw, err := base64.StdEncoding.DecodeString(inlineArg)
		if err != nil {
			return errEnvelope("artifact_publish", fmt.Errorf("base64 decode: %w", err), "invalid_inline_bytes")
		}
		pubReq.Source = PublishSource{InlineBytes: raw}
	default:
		return errEnvelope("artifact_publish", ErrSourceAmbiguous, "source_ambiguous")
	}

	ref, err := t.m.Publish(ctx, pubReq)
	if err != nil {
		return errEnvelope("artifact_publish", err, classifyError(err))
	}
	return map[string]any{
		"id":          ref.ID,
		"name":        ref.Name,
		"type":        ref.Type,
		"visibility":  string(ref.Visibility),
		"size_bytes":  ref.SizeBytes,
		"tags":        ref.Tags,
	}, nil
}

// ─────────────────────────────────────────────────────────────────
// artifact_info
// ─────────────────────────────────────────────────────────────────

type artifactInfoTool struct {
	m *Manager
}

func (t *artifactInfoTool) Name() string { return "artifact_info" }

func (t *artifactInfoTool) Description() string {
	return "Returns the metadata for an artifact you have visibility into: name, type, size, description, tags, schema, and lineage. Use this before artifact_query to confirm the artifact still exists and inspect its file_schema. Returns {error, code: unknown_artifact} when the id is missing or invisible to you."
}

func (t *artifactInfoTool) IsLongRunning() bool { return false }

func (t *artifactInfoTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: "OBJECT",
			Properties: map[string]*genai.Schema{
				"id": {
					Type:        "STRING",
					Description: "Artifact id (e.g. art_ag01_<unix>_<rnd>).",
				},
			},
			Required: []string{"id"},
		},
	}
}

func (t *artifactInfoTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *artifactInfoTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	m, ok := args.(map[string]any)
	if !ok {
		return errEnvelope("artifact_info", fmt.Errorf("unexpected args type %T", args), "invalid_args")
	}
	id := stringArg(m, "id")
	if id == "" {
		return errEnvelope("artifact_info", fmt.Errorf("id required"), "invalid_args")
	}
	detail, err := t.m.Info(ctx, ctx.SessionID(), id)
	if err != nil {
		return errEnvelope("artifact_info", err, classifyError(err))
	}
	out := map[string]any{
		"id":              detail.ID,
		"name":            detail.Name,
		"type":            detail.Type,
		"visibility":      string(detail.Visibility),
		"size_bytes":      detail.SizeBytes,
		"description":     detail.Description,
		"ttl":             string(detail.TTL),
		"storage_backend": detail.StorageBackend,
		"created_at":      detail.CreatedAt,
	}
	if len(detail.Tags) > 0 {
		out["tags"] = detail.Tags
	}
	if detail.OriginalPath != "" {
		out["original_path"] = detail.OriginalPath
	}
	if detail.SessionID != "" {
		out["session_id"] = detail.SessionID
	}
	if detail.DerivedFrom != "" {
		out["derived_from"] = detail.DerivedFrom
	}
	if detail.RowCount != nil {
		out["row_count"] = *detail.RowCount
	}
	if detail.ColCount != nil {
		out["col_count"] = *detail.ColCount
	}
	if len(detail.FileSchema) > 0 {
		out["file_schema"] = detail.FileSchema
	}
	return out, nil
}

// ─────────────────────────────────────────────────────────────────
// artifact_list
// ─────────────────────────────────────────────────────────────────

type artifactListTool struct {
	m *Manager
}

func (t *artifactListTool) Name() string { return "artifact_list" }

func (t *artifactListTool) Description() string {
	return "Lists artifacts your session can see (own + parent-scope + graph-scope + explicit grants + world). Optional filters by `type`, `tags` (AND), and `limit` (default 50, max 200). Returns {artifacts: [...], count: N}."
}

func (t *artifactListTool) IsLongRunning() bool { return false }

func (t *artifactListTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: "OBJECT",
			Properties: map[string]*genai.Schema{
				"type":  {Type: "STRING", Description: "Filter by type (csv | parquet | …)."},
				"tags":  {Type: "ARRAY", Items: &genai.Schema{Type: "STRING"}, Description: "Tags ALL artifacts must carry."},
				"limit": {Type: "INTEGER", Description: "Result cap (default 50, max 200)."},
			},
		},
	}
}

func (t *artifactListTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *artifactListTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	m, ok := args.(map[string]any)
	if !ok {
		return errEnvelope("artifact_list", fmt.Errorf("unexpected args type %T", args), "invalid_args")
	}
	filter := ListFilter{
		Type:  stringArg(m, "type"),
		Tags:  stringSliceArg(m, "tags"),
		Limit: intArg(m, "limit"),
	}
	refs, err := t.m.ListVisible(ctx, ctx.SessionID(), filter)
	if err != nil {
		return errEnvelope("artifact_list", err, classifyError(err))
	}
	out := make([]map[string]any, 0, len(refs))
	for _, r := range refs {
		entry := map[string]any{
			"id":         r.ID,
			"name":       r.Name,
			"type":       r.Type,
			"visibility": string(r.Visibility),
			"size_bytes": r.SizeBytes,
		}
		if len(r.Tags) > 0 {
			entry["tags"] = r.Tags
		}
		if r.DistanceToQuery != nil {
			entry["distance_to_query"] = *r.DistanceToQuery
		}
		out = append(out, entry)
	}
	return map[string]any{"artifacts": out, "count": len(out)}, nil
}

// ─────────────────────────────────────────────────────────────────
// artifact_visibility
// ─────────────────────────────────────────────────────────────────

type artifactVisibilityTool struct {
	m *Manager
}

func (t *artifactVisibilityTool) Name() string { return "artifact_visibility" }

func (t *artifactVisibilityTool) Description() string {
	return "Coordinator-only. Widens an artifact's visibility scope (self → parent → graph → user) and/or grants explicit access to (target_agent_id, target_session_id). Cannot narrow. Sub-agents calling this get {error, code: 'not_coordinator'}."
}

func (t *artifactVisibilityTool) IsLongRunning() bool { return false }

func (t *artifactVisibilityTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: "OBJECT",
			Properties: map[string]*genai.Schema{
				"id":                {Type: "STRING", Description: "Artifact id."},
				"visibility":        {Type: "STRING", Description: "New visibility level (must be wider than current).", Enum: []string{"self", "parent", "graph", "user"}},
				"target_agent_id":   {Type: "STRING", Description: "Optional: grant target's agent id (defaults to current agent)."},
				"target_session_id": {Type: "STRING", Description: "Optional: grant target's session id."},
			},
			Required: []string{"id"},
		},
	}
}

func (t *artifactVisibilityTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *artifactVisibilityTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	m, ok := args.(map[string]any)
	if !ok {
		return errEnvelope("artifact_visibility", fmt.Errorf("unexpected args type %T", args), "invalid_args")
	}
	id := stringArg(m, "id")
	if id == "" {
		return errEnvelope("artifact_visibility", fmt.Errorf("id required"), "invalid_args")
	}
	var vis Visibility
	if v := stringArg(m, "visibility"); v != "" {
		vis = Visibility(v)
	}
	var target *GrantTarget
	if tsid := stringArg(m, "target_session_id"); tsid != "" {
		target = &GrantTarget{
			AgentID:   stringArg(m, "target_agent_id"),
			SessionID: tsid,
		}
	}
	if vis == "" && target == nil {
		return errEnvelope("artifact_visibility",
			fmt.Errorf("at least one of `visibility` or `target_session_id` required"), "invalid_args")
	}
	if err := t.m.WidenVisibility(ctx, ctx.SessionID(), id, vis, target); err != nil {
		return errEnvelope("artifact_visibility", err, classifyError(err))
	}
	return map[string]any{"id": id, "ok": true}, nil
}

// ─────────────────────────────────────────────────────────────────
// artifact_remove
// ─────────────────────────────────────────────────────────────────

type artifactRemoveTool struct {
	m *Manager
}

func (t *artifactRemoveTool) Name() string { return "artifact_remove" }

func (t *artifactRemoveTool) Description() string {
	return "Removes an artifact (bytes + metadata + grants) from the registry. You may remove your own artifacts; coordinators may also remove user-visibility artifacts. Returns {error, code: 'not_authorised'} otherwise."
}

func (t *artifactRemoveTool) IsLongRunning() bool { return false }

func (t *artifactRemoveTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: "OBJECT",
			Properties: map[string]*genai.Schema{
				"id": {Type: "STRING", Description: "Artifact id."},
			},
			Required: []string{"id"},
		},
	}
}

func (t *artifactRemoveTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *artifactRemoveTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	m, ok := args.(map[string]any)
	if !ok {
		return errEnvelope("artifact_remove", fmt.Errorf("unexpected args type %T", args), "invalid_args")
	}
	id := stringArg(m, "id")
	if id == "" {
		return errEnvelope("artifact_remove", fmt.Errorf("id required"), "invalid_args")
	}
	if err := t.m.Remove(ctx, ctx.SessionID(), id); err != nil {
		return errEnvelope("artifact_remove", err, classifyError(err))
	}
	return map[string]any{"id": id, "removed": true}, nil
}

// intArg pulls an int arg out of the LLM's `map[string]any` args,
// defaulting to 0 when missing. JSON numbers come through as float64.
func intArg(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

// ─────────────────────────────────────────────────────────────────
// shared envelope helpers
// ─────────────────────────────────────────────────────────────────

// errEnvelope returns the (envelope, nil) pair the LLM tool surface
// expects: envelope-as-success so the coordinator's mission is not
// aborted by a tool-level failure. Caller passes a short machine
// code matching contracts/artifact-tools.md §Common envelope shape.
func errEnvelope(toolName string, err error, code string) (map[string]any, error) {
	return map[string]any{
		"error": fmt.Sprintf("%s: %s", toolName, err.Error()),
		"code":  code,
	}, nil
}

// classifyError maps a domain sentinel to its envelope code.
func classifyError(err error) string {
	switch {
	case errors.Is(err, ErrUnknownArtifact):
		return "unknown_artifact"
	case errors.Is(err, ErrDescriptionRequired):
		return "description_required"
	case errors.Is(err, ErrSourceAmbiguous):
		return "source_ambiguous"
	case errors.Is(err, ErrInlineBytesTooLarge):
		return "inline_bytes_too_large"
	case errors.Is(err, ErrInvalidVisibility):
		return "invalid_visibility"
	case errors.Is(err, ErrInvalidTTL):
		return "invalid_ttl"
	case errors.Is(err, ErrVisibilityNarrowing):
		return "visibility_narrowing"
	case errors.Is(err, ErrNotCoordinator):
		return "not_coordinator"
	case errors.Is(err, ErrNotAuthorisedToRemove):
		return "not_authorised"
	case errors.Is(err, ErrLocalPathUnavailable):
		return "local_path_unavailable"
	case errors.Is(err, ErrUnregisteredBackend):
		return "backend_not_registered"
	default:
		// Backend-level ErrNotImplemented (s3 stub) bubbles through
		// without one of our sentinels — match on substring as a
		// fallback. The error chain still contains storage.ErrNotImplemented
		// but we don't import storage in this file just for an
		// errors.Is — the substring is unique enough.
		if err != nil && containsAny(err.Error(), "backend not implemented", "not_implemented") {
			return "backend_not_implemented"
		}
		return "internal"
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) == 0 {
			continue
		}
		if len(sub) > len(s) {
			continue
		}
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
	}
	return false
}

// stringArg pulls a string arg out of the LLM's `map[string]any`
// args, defaulting to "" when missing or wrong type.
func stringArg(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// stringSliceArg pulls a []string out of an `[]any` of strings.
// Defaults to nil for any missing / wrong-type arg.
func stringSliceArg(m map[string]any, key string) []string {
	raw, ok := m[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		if s, _ := e.(string); s != "" {
			out = append(out, s)
		}
	}
	return out
}
