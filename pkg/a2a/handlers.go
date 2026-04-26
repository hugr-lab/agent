// Package a2a builds HTTP handlers for the A2A (agent-to-agent) JSON-RPC
// transport: the agent card under /.well-known/agent.json and the
// /invoke endpoint wired through ADK's adka2a executor.
//
// The runtime owns listener lifecycle and orchestration; this package
// stays a pure helper library so it can be mounted on either an
// http.ServeMux (A2A mode) or a gorilla/mux router (devui mode).
package a2a

import (
	"net/http"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/artifact"
	adkplugin "google.golang.org/adk/plugin"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/server/adka2a"
	adksession "google.golang.org/adk/session"
)

// BuildHandlers returns the two HTTP handlers that make up the A2A
// surface: the static agent card and the JSON-RPC invoke endpoint.
//
// baseURL is the externally-visible URL prefix the agent card
// advertises for /invoke (e.g. "http://localhost:10000"). It must be
// reachable from A2A clients — in devui mode this is the *A2A*
// listener base URL, not the DevUI listener.
//
// userUploadPlugin (optional) intercepts incoming user messages and
// converts FilePart{FileBytes} (decoded by adka2a as
// genai.Part{InlineData}) into artifacts via the artifact registry,
// replacing the part with a rich text placeholder that names the
// artifact id, MIME type, size, and (when available) a local
// filesystem path the model can hand to python/duckdb/curl tools.
// Pass nil to skip plugin wiring (uploads then flow as inline bytes
// to the LLM — useful for tests).
func BuildHandlers(
	a agent.Agent,
	sessionSvc adksession.Service,
	artifactSvc artifact.Service,
	userUploadPlugin *adkplugin.Plugin,
	baseURL string,
) (card, invoke http.Handler) {
	agentCard := &a2a.AgentCard{
		Name:        a.Name(),
		Description: a.Description(),
		// DefaultInputModes advertises the MIME types the agent
		// accepts on incoming A2A FilePart{FileBytes}. The runner's
		// ingest path (RunConfig.SaveInputBlobsAsArtifacts below)
		// auto-publishes any FilePart the client sends regardless of
		// MIME — this list is just the discovery surface for clients
		// that ask "can I send a CSV / a PDF here". Common types
		// listed; clients are free to send other media types.
		DefaultInputModes: []string{
			"text/plain",
			"text/csv",
			"text/markdown",
			"application/json",
			"application/pdf",
			"application/octet-stream",
		},
		DefaultOutputModes: []string{"text/plain"},
		URL:                baseURL + "/invoke",
		PreferredTransport: a2a.TransportProtocolJSONRPC,
		Skills:             adka2a.BuildAgentSkills(a),
		Capabilities:       a2a.AgentCapabilities{Streaming: true},
	}
	runnerCfg := runner.Config{
		AppName:         a.Name(),
		Agent:           a,
		SessionService:  sessionSvc,
		ArtifactService: artifactSvc,
	}
	if userUploadPlugin != nil {
		// Plugin runs inside runner.appendMessageToSession BEFORE
		// the SaveInputBlobsAsArtifacts block (runner.go:274-291)
		// and BEFORE the user_message event lands. Our plugin
		// publishes each InlineData part through the artifact
		// registry and replaces it with a rich text placeholder
		// that exposes artifact id + MIME + size + local path —
		// the LLM then has enough metadata to drive python/duckdb
		// /curl tools at the file directly. With the plugin
		// enabled, SaveInputBlobsAsArtifacts stays false (no point
		// — the runner finds no remaining InlineData parts to
		// auto-Save).
		runnerCfg.PluginConfig = runner.PluginConfig{
			Plugins: []*adkplugin.Plugin{userUploadPlugin},
		}
	}
	executor := adka2a.NewExecutor(adka2a.ExecutorConfig{
		RunnerConfig: runnerCfg,
	})
	return a2asrv.NewStaticAgentCardHandler(agentCard),
		a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(executor))
}
