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
