package classifier

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hugr-lab/hugen/pkg/store"
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

// DefaultBuffer is the channel capacity used when the caller passes 0.
// Sized for a single-process agent running typical conversations at
// ~4 events/turn: 1024 = ~250 turns of head-room before drop-on-full.
const DefaultBuffer = 1024

// Classifier drains a buffered envelope channel, maps each ADK event
// to one or more session_events rows, and writes them to HubDB.
//
// Zero goroutines start until Run(ctx) is called. Publish() may be
// invoked from any goroutine (including the producer Session), never
// blocks, and drops events silently when the channel is full.
type Classifier struct {
	hub    store.DB
	logger *slog.Logger

	ch      chan Envelope
	dropped atomic.Int64

	stopped chan struct{}
	stopMu  sync.Mutex
	closed  bool
}

// New constructs a Classifier with the given channel capacity. Pass 0
// for DefaultBuffer.
func New(hub store.DB, logger *slog.Logger, bufSize int) *Classifier {
	if logger == nil {
		logger = slog.Default()
	}
	if bufSize <= 0 {
		bufSize = DefaultBuffer
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
func (c *Classifier) handle(ctx context.Context, env Envelope) {
	rows := Classify(env)
	for _, row := range rows {
		row.SessionID = env.SessionID
		if _, err := c.hub.AppendEvent(ctx, row); err != nil {
			c.logger.Warn("classifier: AppendEvent failed",
				"session", env.SessionID, "type", row.EventType, "err", err)
		}
	}
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
func Classify(env Envelope) []store.SessionEvent {
	ev := env.Event
	if ev == nil {
		return nil
	}
	if ev.Partial {
		return nil
	}
	var rows []store.SessionEvent

	// Function calls / responses first — ADK emits them on both the
	// user and the agent side of a tool invocation.
	if ev.Content != nil {
		for _, part := range ev.Content.Parts {
			if part == nil {
				continue
			}
			if fc := part.FunctionCall; fc != nil {
				row := store.SessionEvent{
					EventType: store.EventTypeToolCall,
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
				rows = append(rows, store.SessionEvent{
					EventType:  store.EventTypeToolResult,
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
			rows = append(rows, store.SessionEvent{
				EventType: store.EventTypeUserMessage,
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
		rows = append(rows, store.SessionEvent{
			EventType: store.EventTypeLLMResponse,
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

// notes keeps a reference to store.SessionEvent so we don't lose
// the import (will be used when classification lands).
var _ = store.SessionEvent{}
