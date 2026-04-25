package executor

import (
	"context"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/missions/graph"
	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
)

// EventWriter is the minimal session-event surface the Executor needs
// to publish mission lifecycle events (agent_spawn, agent_result,
// agent_abstained). Satisfied by *sessstore.Client in production; a
// tiny fake in tests.
type EventWriter interface {
	AppendEvent(ctx context.Context, ev sessstore.Event) (string, error)
	AppendEventWithSummary(ctx context.Context, ev sessstore.Event, summary string) (string, error)
}

// emitSpawn writes one agent_spawn row on the coordinator session.
// Fires when the Executor promotes a mission to running. Exactly
// once per mission per lifetime — never re-emitted across restarts.
func (e *Executor) emitSpawn(ctx context.Context, coordID string, node *missionNode) string {
	meta := map[string]any{
		"mission_id": node.id,
		"skill":      node.skill,
		"role":       node.role,
		"task":       node.task,
	}
	id, err := e.events.AppendEvent(ctx, sessstore.Event{
		SessionID: coordID,
		EventType: sessstore.EventTypeAgentSpawn,
		Author:    "agent",
		Content:   fmt.Sprintf("Spawning %s/%s: %s", node.skill, node.role, truncate(node.task, 120)),
		Metadata:  meta,
	})
	if err != nil {
		e.logger.WarnContext(ctx, "missions: emit agent_spawn", "coord", coordID, "err", err)
	}
	return id
}

// emitResult writes one agent_result row on the coordinator session
// when the mission reaches any terminal status. Summary flows through
// the `summary:` path so Hugr embeds it server-side — phase-4 search
// finds missions by their outcome.
func (e *Executor) emitResult(ctx context.Context, node *missionNode, res missionResult) {
	persistedStatus := runtimeToPersistedStatus(node.status)
	durMs := int64(0)
	if !node.terminated.IsZero() && !node.startedAt.IsZero() {
		durMs = node.terminated.Sub(node.startedAt).Milliseconds()
	}
	meta := map[string]any{
		"mission_id":  node.id,
		"status":      persistedStatus,
		"turns_used":  node.turnsUsed,
		"summary":     res.summary,
		"duration_ms": durMs,
	}
	if node.reason != "" {
		meta["reason"] = node.reason
	}
	content := fmt.Sprintf("%s/%s: %s — %s", node.skill, node.role, persistedStatus, truncate(res.summary, 240))
	ev := sessstore.Event{
		SessionID: node.coordID,
		EventType: sessstore.EventTypeAgentResult,
		Author:    "agent",
		Content:   content,
		Metadata:  meta,
	}
	if _, err := e.events.AppendEventWithSummary(ctx, ev, res.summary); err != nil {
		e.logger.WarnContext(ctx, "missions: emit agent_result", "id", node.id, "err", err)
	}
}

// emitAbstained writes one agent_abstained row on the coordinator —
// additive to agent_result when the sub-agent's final message was a
// refusal ("I can't help…"). Audit-only, not eligible for embedding.
func (e *Executor) emitAbstained(ctx context.Context, node *missionNode, reason string) {
	meta := map[string]any{"mission_id": node.id, "reason": reason}
	_, err := e.events.AppendEvent(ctx, sessstore.Event{
		SessionID: node.coordID,
		EventType: sessstore.EventTypeAgentAbstained,
		Author:    "agent",
		Content:   fmt.Sprintf("%s/%s abstained: %s", node.skill, node.role, truncate(reason, 160)),
		Metadata:  meta,
	})
	if err != nil {
		e.logger.WarnContext(ctx, "missions: emit agent_abstained", "id", node.id, "err", err)
	}
}

// runtimeToPersistedStatus maps Executor's in-memory Status taxonomy
// to the `sessions.status` column values. Duplicated from store/
// here to keep executor/ independent of store/ for this tiny helper.
func runtimeToPersistedStatus(runtime string) string {
	switch runtime {
	case graph.StatusDone:
		return "completed"
	case graph.StatusFailed:
		return "failed"
	case graph.StatusAbandoned:
		return "abandoned"
	default:
		return runtime
	}
}

func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
