package sessions

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
	"github.com/hugr-lab/query-engine/types"
	adksession "google.golang.org/adk/session"
)

// maxToolResultBytes caps the tool_result payload persisted in
// session_events.tool_result. Overflow is replaced with a pointer
// placeholder — the full payload is not needed for review because the
// agent already summarised it in its own turn.
const maxToolResultBytes = 64 * 1024

// Envelope is the channel payload: an ADK event plus the session it
// belongs to. The Session struct pushes one per IngestADKEvent call.
type Envelope struct {
	SessionID string
	Event     *adksession.Event
}

// DefaultClassifierBuffer is the channel capacity used when the caller passes 0.
// Sized for a single-process agent running typical conversations at
// ~4 events/turn: 1024 = ~250 turns of head-room before drop-on-full.
const DefaultClassifierBuffer = 1024

// Classifier drains a buffered envelope channel, maps each ADK event
// to one or more session_events rows, and writes them to HubDB.
//
// Zero goroutines start until Run(ctx) is called. Publish() may be
// invoked from any goroutine (including the producer Session), never
// blocks, and drops events silently when the channel is full.
type Classifier struct {
	hub    *sessstore.Client
	logger *slog.Logger

	// manager is an optional *Manager reference used to route
	// transcript writes through Session.AppendEvent so the per-session
	// write lock serialises classifier events against synchronous
	// writers (LoadSkill, compactor). Left nil in standalone tests —
	// writes go direct to hub with best-effort seq.
	manager *Manager

	ch      chan Envelope
	dropped atomic.Int64

	// inflight tracks envelopes that have been accepted onto the
	// channel but not yet fully processed by handle(). Flush() polls
	// this to wait for queue quiescence without closing the channel.
	inflight atomic.Int64

	stopped chan struct{}
	stopMu  sync.Mutex
	closed  bool
}

// AttachManager wires the Classifier to the session manager so
// transcript writes take the per-session lock. Safe to call once,
// after construction, before Run(ctx). No-op when called repeatedly.
func (c *Classifier) AttachManager(m *Manager) {
	if c.manager == nil {
		c.manager = m
	}
}

// NewClassifier constructs a Classifier with the given channel capacity.
// Pass 0 for DefaultClassifierBuffer. The classifier builds its own
// sessstore client internally from the given querier + identity. A nil
// querier yields a no-op classifier (Run() drains the channel without
// hub writes) — useful for tests.
func NewClassifier(querier types.Querier, agentID, agentShort string, logger *slog.Logger, bufSize int) *Classifier {
	if querier == nil {
		return NewClassifierWithHub(nil, logger, bufSize)
	}
	hub, err := sessstore.New(querier, sessstore.Options{
		AgentID: agentID, AgentShort: agentShort, Logger: logger,
	})
	if err != nil {
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("classifier: build sessions store failed, running without hub persistence", "err", err)
		return NewClassifierWithHub(nil, logger, bufSize)
	}
	return NewClassifierWithHub(hub, logger, bufSize)
}

// NewClassifierWithHub builds a Classifier on top of an already-
// constructed sessions-store client. Preferred wiring from runtime.go
// so every subsystem shares the same *sessstore.Client instance.
func NewClassifierWithHub(hub *sessstore.Client, logger *slog.Logger, bufSize int) *Classifier {
	if logger == nil {
		logger = slog.Default()
	}
	if bufSize <= 0 {
		bufSize = DefaultClassifierBuffer
	}
	return &Classifier{
		hub:     hub,
		logger:  logger,
		ch:      make(chan Envelope, bufSize),
		stopped: make(chan struct{}),
	}
}

// C returns the send-side of the envelope channel. Sessions use it
// for non-blocking Publish attempts; tests can inject events directly.
func (c *Classifier) C() chan<- Envelope { return c.ch }

// Publish tries to push an envelope onto the classifier channel.
// Non-blocking: if the channel is full the event is dropped and the
// dropped counter is bumped. Returns true iff the event was queued.
func (c *Classifier) Publish(env Envelope) bool {
	select {
	case c.ch <- env:
		c.inflight.Add(1)
		return true
	default:
		n := c.dropped.Add(1)
		// WARN the first drop after every 100 drops to bound log
		// volume while still making the pathology visible.
		if n == 1 || n%100 == 0 {
			c.logger.Warn("classifier: queue full, dropped event",
				"session", env.SessionID,
				"dropped_total", n)
		}
		return false
	}
}

// Dropped returns the total count of events dropped because the
// channel was full.
func (c *Classifier) Dropped() int64 { return c.dropped.Load() }

// Run drains the channel until ctx is cancelled. Exits after writing
// the shutdown signal to Done(). Each drained envelope is classified
// and one or more session_events rows are written via the hub
// adapter; errors are logged at WARN level without failing the loop.
func (c *Classifier) Run(ctx context.Context) {
	defer close(c.stopped)
	c.logger.Info("classifier started", "buffer", cap(c.ch))
	for {
		select {
		case <-ctx.Done():
			c.logger.Info("classifier stopping",
				"dropped_total", c.dropped.Load(),
				"queued", len(c.ch))
			return
		case env, ok := <-c.ch:
			if !ok {
				return
			}
			c.handle(ctx, env)
		}
	}
}

// handle classifies a single envelope and persists any rows it
// produced. Errors are logged, never returned — the classifier
// consumer loop is best-effort.
//
// Spec 006 §2: eligible event types carry a `summary:` payload that
// Hugr embeds server-side on insert. Embedder failures surface as
// mutation errors — the classifier falls back to a plain insert
// (empty summary → NULL embedding) so the transcript row always
// lands, and a WARN log points a backfill worker at the gap.
func (c *Classifier) handle(ctx context.Context, env Envelope) {
	defer c.inflight.Add(-1)
	rows := Classify(env)
	if c.hub == nil && c.manager == nil {
		return
	}
	for _, row := range rows {
		row.SessionID = env.SessionID
		summary := buildSummary(row)
		_, err := c.appendEvent(ctx, env.SessionID, row, summary)
		if err == nil {
			continue
		}
		if summary == "" {
			c.logger.Warn("classifier: AppendEvent failed",
				"session", env.SessionID, "type", row.EventType, "err", err)
			continue
		}
		// Embedder-or-other failure on the summary path: retry once
		// WITHOUT summary so the row still persists with NULL embedding.
		c.logger.Warn("classifier: embed insert failed, retrying without summary",
			"session", env.SessionID, "type", row.EventType, "err", err)
		if _, err := c.appendEvent(ctx, env.SessionID, row, ""); err != nil {
			c.logger.Warn("classifier: AppendEvent retry failed",
				"session", env.SessionID, "type", row.EventType, "err", err)
		}
	}
}

// appendEvent routes the write through Manager (session-scoped lock)
// when available, and falls back to the bare hub client otherwise.
// Tests that construct a Classifier with hub only keep the plain path.
func (c *Classifier) appendEvent(ctx context.Context, sessionID string, row sessstore.Event, summary string) (string, error) {
	if c.manager != nil {
		return c.manager.PublishHubEvent(ctx, sessionID, row, summary)
	}
	return c.hub.AppendEventWithSummary(ctx, row, summary)
}

// buildSummary returns the text Hugr should embed for the given row,
// or "" when the event type is ineligible (spec 006 contract
// session-events-semantic.md §2 eligibility table).
//
// | type                  | payload                                     |
// |-----------------------|---------------------------------------------|
// | user_message          | row.Content                                 |
// | llm_response          | row.Content                                 |
// | agent_message         | row.Content (phase-2 producer)              |
// | tool_call             | fmt "%s(%s)" name + compact args, ≤200 runes|
// | tool_result           | row.ToolResult, non-empty ≤ 64 KiB, first 2048|
// | compaction_summary    | row.Content                                 |
// | agent_result (phase2) | metadata.summary                            |
// | other                 | ""                                          |
func buildSummary(row sessstore.Event) string {
	switch row.EventType {
	case sessstore.EventTypeUserMessage,
		sessstore.EventTypeLLMResponse,
		sessstore.EventTypeAgentMessage,
		sessstore.EventTypeCompactionSummary:
		return row.Content
	case sessstore.EventTypeToolCall:
		return toolCallDigest(row.ToolName, row.ToolArgs)
	case sessstore.EventTypeToolResult:
		if row.ToolResult == "" {
			return ""
		}
		// AppendEvent already caps the persisted payload at
		// maxToolResultBytes (64 KiB). The embedding summary is an
		// even tighter window — the first 2 048 runes stay cheap for
		// the embedder call without losing the tool's intent.
		return truncateRunes(row.ToolResult, 2048)
	case sessstore.EventTypeAgentResult:
		if row.Metadata == nil {
			return ""
		}
		if v, ok := row.Metadata["summary"].(string); ok {
			return v
		}
		return ""
	default:
		return ""
	}
}

// toolCallDigest renders a one-line summary of a tool_call row for
// embedding: `<tool_name>(<compact args>)`. Args are marshalled to
// JSON and truncated at 200 runes to stay cheap.
func toolCallDigest(name string, args map[string]any) string {
	if name == "" && len(args) == 0 {
		return ""
	}
	argsJSON := ""
	if len(args) > 0 {
		if b, err := json.Marshal(args); err == nil {
			argsJSON = string(b)
		}
	}
	out := name + "(" + argsJSON + ")"
	return truncateRunes(out, 200)
}

func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// Classify maps a single ADK event to zero or more session_events
// rows. Exported so other packages (reviewer tests, session manager
// restore paths) can reuse the mapping without spinning up a real
// classifier goroutine.
//
// Rules (spec 005 data-model §Classification rules):
//   - Partial events are dropped.
//   - User turns → 1 user_message row.
//   - Agent terminal turns → 1 llm_response row (with truncated=true
//     when the stream was interrupted / cancelled).
//   - Every Part carrying a FunctionCall → 1 tool_call row.
//   - Every Part carrying a FunctionResponse → 1 tool_result row,
//     with tool_result capped at maxToolResultBytes.
func Classify(env Envelope) []sessstore.Event {
	ev := env.Event
	if ev == nil {
		return nil
	}
	if ev.Partial {
		return nil
	}
	var rows []sessstore.Event

	// Function calls / responses first — ADK emits them on both the
	// user and the agent side of a tool invocation.
	if ev.Content != nil {
		for _, part := range ev.Content.Parts {
			if part == nil {
				continue
			}
			if fc := part.FunctionCall; fc != nil {
				row := sessstore.Event{
					EventType: sessstore.EventTypeToolCall,
					Author:    nonEmpty(ev.Author, "agent"),
					ToolName:  fc.Name,
					ToolArgs:  fc.Args,
				}
				rows = append(rows, row)
			}
			if fr := part.FunctionResponse; fr != nil {
				body := marshalToolResponse(fr.Response)
				truncated := false
				if len(body) > maxToolResultBytes {
					body = body[:maxToolResultBytes] + "…[truncated]"
					truncated = true
				}
				meta := map[string]any{"tool": fr.Name}
				if truncated {
					meta["truncated"] = true
				}
				rows = append(rows, sessstore.Event{
					EventType:  sessstore.EventTypeToolResult,
					Author:     "tool",
					ToolName:   fr.Name,
					ToolResult: body,
					Metadata:   meta,
				})
			}
		}
	}

	text := joinText(ev)
	author := ev.Author
	role := ""
	if ev.Content != nil {
		role = ev.Content.Role
	}
	switch {
	case role == "user" || author == "user":
		if text != "" {
			rows = append(rows, sessstore.Event{
				EventType: sessstore.EventTypeUserMessage,
				Author:    "user",
				Content:   text,
			})
		}
	default:
		// Agent-side terminal: emit a llm_response if there is text
		// OR if the stream was interrupted (so review knows the turn
		// ended abruptly).
		if text == "" && !ev.Interrupted {
			break
		}
		meta := map[string]any{}
		if ev.Interrupted {
			meta["truncated"] = true
		}
		if ev.ModelVersion != "" {
			meta["model"] = ev.ModelVersion
		}
		if ev.UsageMetadata != nil {
			meta["prompt_tokens"] = int(ev.UsageMetadata.PromptTokenCount)
			meta["completion_tokens"] = int(ev.UsageMetadata.CandidatesTokenCount)
		}
		rows = append(rows, sessstore.Event{
			EventType: sessstore.EventTypeLLMResponse,
			Author:    nonEmpty(author, "agent"),
			Content:   text,
			Metadata:  meta,
		})
	}
	return rows
}

func joinText(ev *adksession.Event) string {
	if ev == nil || ev.Content == nil {
		return ""
	}
	var out string
	for _, p := range ev.Content.Parts {
		if p == nil {
			continue
		}
		if p.Text != "" {
			if out != "" {
				out += "\n"
			}
			out += p.Text
		}
	}
	return out
}

func marshalToolResponse(resp map[string]any) string {
	if resp == nil {
		return ""
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return ""
	}
	return string(b)
}

func nonEmpty(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// Done returns a channel closed when Run() exits.
func (c *Classifier) Done() <-chan struct{} { return c.stopped }

// Flush waits up to `budget` for every envelope currently in-flight
// (queued + being handled) to finish processing, WITHOUT closing the
// channel. Use this between scenario steps / before polling the
// session_events table to make sure async-classified events have
// been persisted. Unlike Drain, the classifier keeps accepting
// Publish calls after Flush returns.
//
// Returns context.DeadlineExceeded on timeout. Safe against concurrent
// Publish: the counter may rise again after return; callers using
// Flush are expected to have no outstanding RunTurn at the moment.
func (c *Classifier) Flush(ctx context.Context, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	for c.inflight.Load() > 0 {
		if time.Now().After(deadline) {
			c.logger.Warn("classifier flush timed out",
				"inflight", c.inflight.Load(),
				"queued", len(c.ch))
			return context.DeadlineExceeded
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

// Drain closes the input channel and waits up to `timeout` for the
// consumer goroutine to finish draining it. Returns context.DeadlineExceeded
// if the wait times out. Safe to call multiple times.
func (c *Classifier) Drain(ctx context.Context, timeout time.Duration) error {
	c.stopMu.Lock()
	if !c.closed {
		close(c.ch)
		c.closed = true
	}
	c.stopMu.Unlock()

	wait := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		wait, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	select {
	case <-c.stopped:
		c.logger.Info("classifier drained",
			"dropped_total", c.dropped.Load())
		return nil
	case <-wait.Done():
		c.logger.Warn("classifier drain timed out",
			"queued", len(c.ch),
			"dropped_total", c.dropped.Load())
		return wait.Err()
	}
}

// notes keeps a reference to sessstore.Event so we don't lose
// the import (will be used when classification lands).
var _ = sessstore.Event{}
