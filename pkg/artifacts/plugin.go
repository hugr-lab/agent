package artifacts

import (
	"errors"
	"fmt"
	"strings"

	"google.golang.org/adk/agent"
	adkplugin "google.golang.org/adk/plugin"
	"google.golang.org/genai"
)

// UserUploadPlugin returns an ADK plugin that auto-publishes user
// file uploads (incoming A2A FilePart{FileBytes} → genai.Part with
// InlineData) into the artifact registry, replacing each blob with
// a rich text placeholder. The placeholder names the artifact id,
// MIME type, byte size, original display name, and — when the
// active storage backend exposes one — a local filesystem path the
// LLM can hand to python / duckdb / curl tools directly.
//
// Why a plugin instead of relying on agent.RunConfig.SaveInputBlobsAsArtifacts:
// the runner's built-in path replaces the blob with the static
// string "Uploaded file: <auto-name>. It has been saved to the
// artifacts" (runner.go:295-308), which doesn't expose our
// artifact id, MIME type, or local path. The plugin runs earlier
// in the same hook (PluginManager.RunOnUserMessageCallback at
// runner.go:274) and substitutes a richer placeholder before the
// auto-Save block ever runs.
//
// Defaults applied to each upload come from the manager's Config:
//   - Visibility = cfg.UploadDefaultVisibility (operator knob)
//   - TTL        = cfg.UploadDefaultTTL        (operator knob)
//
// FileURI parts (genai.Part.FileData) are NOT auto-published —
// the runner ignores them too, and ADK's design treats URIs as
// "the model can fetch this itself if it wants". They flow through
// to the LLM as-is.
func (m *Manager) UserUploadPlugin() (*adkplugin.Plugin, error) {
	return adkplugin.New(adkplugin.Config{
		Name:                  "artifacts-user-upload",
		OnUserMessageCallback: m.onUserMessage,
	})
}

// onUserMessage is the OnUserMessageCallback body. Mutates msg
// in place — for each InlineData part, publishes via Manager and
// replaces the part with a TextPart that carries the artifact's
// metadata header.
//
// Errors from a single publish do not abort the whole turn; we log
// and leave the offending part untouched (the LLM will see the
// raw blob and produce its own error). Aborting would deny service
// for the entire user turn over a single bad upload.
func (m *Manager) onUserMessage(ctx agent.InvocationContext, msg *genai.Content) (*genai.Content, error) {
	if msg == nil || len(msg.Parts) == 0 {
		return nil, nil
	}
	sess := ctx.Session()
	if sess == nil {
		return nil, errors.New("artifacts: user-upload plugin: invocation has no session")
	}
	sessionID := sess.ID()
	if sessionID == "" {
		return nil, errors.New("artifacts: user-upload plugin: empty session id")
	}

	mutated := false
	for i, part := range msg.Parts {
		if part == nil || part.InlineData == nil || len(part.InlineData.Data) == 0 {
			continue
		}
		blob := part.InlineData
		displayName := blob.DisplayName
		if displayName == "" {
			displayName = fmt.Sprintf("upload_%d", i)
		}
		req := PublishRequest{
			CallerSessionID: sessionID,
			Source:          PublishSource{InlineBytes: blob.Data},
			Name:            displayName,
			Type:            typeFromMIME(blob.MIMEType),
			Description:     "User-uploaded " + displayName,
			Visibility:      m.cfg.UploadDefaultVisibility,
			TTL:             m.cfg.UploadDefaultTTL,
			EventSource:     "user_upload",
		}
		ref, err := m.Publish(ctx, req)
		if err != nil {
			m.log.Warn("artifacts: user-upload plugin: publish failed",
				"session_id", sessionID, "name", displayName, "err", err)
			continue
		}
		localPath, _, lpErr := m.ResolveLocalPath(ctx, ref.ID)
		if lpErr != nil {
			m.log.Warn("artifacts: user-upload plugin: ResolveLocalPath",
				"artifact_id", ref.ID, "err", lpErr)
			localPath = ""
		}
		mime := blob.MIMEType
		if mime == "" {
			mime = mimeFromType(ref.Type)
		}
		msg.Parts[i] = &genai.Part{Text: formatUploadPlaceholder(ref, mime, localPath)}
		mutated = true
	}
	if !mutated {
		return nil, nil
	}
	return msg, nil
}

// formatUploadPlaceholder builds the text block the LLM sees in
// place of an InlineData part. Format choices:
//   - Markdown-ish so a casual reader still understands it.
//   - One field per line, colon-separated, so prompt-truncation
//     leaves at least the artifact id readable.
//   - Local path is the headline for tool-driven analysis: stating
//     it as `local_path` rather than as a URL keeps the model from
//     trying to GET it over HTTP.
func formatUploadPlaceholder(ref ArtifactRef, mime, localPath string) string {
	var b strings.Builder
	b.WriteString("[user-upload]\n")
	fmt.Fprintf(&b, "artifact_id: %s\n", ref.ID)
	fmt.Fprintf(&b, "name: %s\n", ref.Name)
	if ref.Type != "" {
		fmt.Fprintf(&b, "type: %s\n", ref.Type)
	}
	if mime != "" {
		fmt.Fprintf(&b, "mime: %s\n", mime)
	}
	if ref.SizeBytes > 0 {
		fmt.Fprintf(&b, "size_bytes: %d\n", ref.SizeBytes)
	}
	if string(ref.Visibility) != "" {
		fmt.Fprintf(&b, "visibility: %s\n", ref.Visibility)
	}
	if localPath != "" {
		fmt.Fprintf(&b, "local_path: %s\n", localPath)
		b.WriteString("# The local_path is mounted on the same host the agent runs on;\n")
		b.WriteString("# pass it directly to python/duckdb/curl tools when available.\n")
	}
	b.WriteString("# Use artifact_info(id) for richer metadata; the bytes are NOT inlined here.")
	return b.String()
}
