//go:build duckdb_arrow && scenario

package harness

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
)

// Inspector reads the hub.db state via the scenario engine. Surfaces
// sessions / events / notes in a form suitable for dumping to t.Log
// so the operator can eyeball post-run state without leaving the
// test output.
type Inspector struct {
	hub    *sessstore.Client
	agent  *Agent
}

// Inspect builds a read-only Inspector over the scenario's hub.db.
// Safe to call multiple times — each returns a fresh client (cheap).
func (a *Agent) Inspect() *Inspector {
	hub, err := sessstore.New(a.Runtime.Querier, sessstore.Options{
		AgentID:         a.AgentID,
		AgentShort:      "ag01",
		Logger:          a.logger,
		EmbedderEnabled: true,
	})
	if err != nil {
		a.logger.Warn("inspector: build sessstore client", "err", err)
		return &Inspector{agent: a}
	}
	return &Inspector{hub: hub, agent: a}
}

// EventCount returns the number of session_events rows for the
// given session, or -1 on error. Used by the scenario runner to
// poll for "no new events in N seconds" without dumping the full
// event list every iteration.
func (in *Inspector) EventCount(ctx context.Context, sessionID string) int {
	if in == nil || in.hub == nil {
		return -1
	}
	evs, err := in.hub.GetEvents(ctx, sessionID)
	if err != nil {
		return -1
	}
	return len(evs)
}

// LatestEvents returns up to `max` most recent events on the session
// (ordered seq ASC). Empty slice on error. Used by the scenario
// runner to poll for "did a specific event type land yet?" without
// pulling the whole transcript on every tick.
func (in *Inspector) LatestEvents(ctx context.Context, sessionID string, max int) []sessstore.Event {
	if in == nil || in.hub == nil {
		return nil
	}
	evs, err := in.hub.GetEvents(ctx, sessionID)
	if err != nil {
		return nil
	}
	if max <= 0 || max >= len(evs) {
		return evs
	}
	return evs[len(evs)-max:]
}

// Sessions returns the root + all subagent sessions keyed by id.
// Order: root first, then children sorted by created_at ASC.
func (in *Inspector) Sessions(ctx context.Context, rootID string) []sessstore.Record {
	if in == nil || in.hub == nil {
		return nil
	}
	root, err := in.hub.GetSession(ctx, rootID)
	if err != nil || root == nil {
		return nil
	}
	children, err := in.hub.ListChildSessions(ctx, rootID)
	if err != nil {
		children = nil
	}
	sort.Slice(children, func(i, j int) bool {
		return children[i].CreatedAt.Before(children[j].CreatedAt)
	})
	out := make([]sessstore.Record, 0, 1+len(children))
	out = append(out, *root)
	out = append(out, children...)
	return out
}

// Events returns every event in a session ordered by seq ASC.
func (in *Inspector) Events(ctx context.Context, sessionID string) []sessstore.Event {
	if in == nil || in.hub == nil {
		return nil
	}
	evs, err := in.hub.GetEvents(ctx, sessionID)
	if err != nil {
		return nil
	}
	return evs
}

// Notes returns the session_notes_chain walk for a session — both
// self-scope notes and any inherited from parents via scope=parent /
// ancestors.
func (in *Inspector) Notes(ctx context.Context, sessionID string) []sessstore.NoteWithDepth {
	if in == nil || in.hub == nil {
		return nil
	}
	notes, err := in.hub.ListNotesChain(ctx, sessionID)
	if err != nil {
		return nil
	}
	return notes
}

// Dump prints a complete hub.db snapshot for rootID into t.Log —
// sessions + each session's events + notes. Large text columns are
// truncated to keep the output readable; the full rows stay on
// disk for duckdb inspection.
func (in *Inspector) Dump(ctx context.Context, t *testing.T, rootID string) {
	t.Helper()
	if in == nil || in.hub == nil {
		t.Log("── DB snapshot ── (inspector disabled)")
		return
	}

	t.Logf("── DB snapshot: hub.db=%s ──", in.agent.HubPath)

	sessions := in.Sessions(ctx, rootID)
	if len(sessions) == 0 {
		t.Logf("   (no sessions found for root %s)", rootID)
		return
	}

	t.Logf("sessions:")
	for _, s := range sessions {
		skill := metaString(s.Metadata, "skill")
		role := metaString(s.Metadata, "role")
		t.Logf("  [%s]  id=%s  parent=%s  status=%-9s  skill=%s  role=%s  mission=%q",
			s.SessionType, short(s.ID), short(s.ParentSessionID), s.Status,
			emptyDash(skill), emptyDash(role), singleLine(s.Mission, 60))
	}

	for _, s := range sessions {
		evs := in.Events(ctx, s.ID)
		t.Logf("session_events %s (%s, n=%d):", short(s.ID), s.SessionType, len(evs))
		for _, ev := range evs {
			tag := ev.EventType
			body := ""
			switch ev.EventType {
			case "tool_call":
				body = fmt.Sprintf("tool=%s args=%s", ev.ToolName, singleLine(toJSONish(ev.ToolArgs), 120))
			case "tool_result":
				body = fmt.Sprintf("tool=%s result=%s", ev.ToolName, singleLine(ev.ToolResult, 120))
			case "skill_loaded", "skill_unloaded":
				body = fmt.Sprintf("skill=%s", ev.Content)
			default:
				body = singleLine(ev.Content, 120)
			}
			t.Logf("   %3d %-20s %s", ev.Seq, tag, body)
		}
	}

	for _, s := range sessions {
		notes := in.Notes(ctx, s.ID)
		if len(notes) == 0 {
			continue
		}
		t.Logf("session_notes visible to %s (n=%d):", short(s.ID), len(notes))
		for _, n := range notes {
			author := ""
			if n.AuthorSessionID != "" && n.AuthorSessionID != n.SessionID {
				author = fmt.Sprintf(" [from=%s]", short(n.AuthorSessionID))
			}
			t.Logf("   • %s%s  %s", n.ID, author, singleLine(n.Content, 160))
		}
	}
}

func short(s string) string {
	if s == "" {
		return "-"
	}
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

func emptyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func metaString(md map[string]any, key string) string {
	if md == nil {
		return ""
	}
	v, ok := md[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

func toJSONish(m map[string]any) string {
	if len(m) == 0 {
		return "{}"
	}
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}
