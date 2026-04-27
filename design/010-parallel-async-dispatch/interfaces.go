// Package design010 sketches the Go interfaces introduced by design 010
// (parallel async dispatch + restart semantics). These types are not
// imported by any production code yet — they exist to anchor the
// design and to give a future implementor a starting shape.
//
// Mapping to runtime packages once implemented:
//   - SessionMessage + Inbox     → pkg/sessions
//   - Dispatcher.SpawnAsync      → pkg/agent/subagent.go
//   - Manager.AppendFunctionResp → pkg/sessions/manager.go
//   - PendingCallsView            → pkg/sessions/store
package design010

import (
	"context"
	"time"
)

// ---------------------------------------------------------------------
// Inbox + messages
// ---------------------------------------------------------------------

// Inbox is the control-plane mailbox owned by every active session
// actor. Capacity is bounded — senders block when an actor is slow,
// which is the desired back-pressure (mirrors ADK ParallelAgent
// ack-per-event semantics).
type Inbox struct {
	msgs chan SessionMessage
	done chan struct{}
}

// Send delivers msg to the actor. Blocks if the inbox is full or the
// actor is slow consuming. Returns an error if the inbox is closed
// (actor goroutine has exited).
func (i *Inbox) Send(ctx context.Context, msg SessionMessage) error { return nil }

// Close marks the inbox closed; subsequent Send calls return an
// error. Idempotent.
func (i *Inbox) Close() {}

// SessionMessage is the sealed message variant. Concrete types below.
type SessionMessage interface{ isSessionMessage() }

// ChildCompleted is emitted by a child session goroutine when its
// runner.Run loop terminates (success / failure / abandoned). The
// parent's actor:
//  1. AppendEvent(parent, function_response{id: CallID, response: Result}).
//  2. Marks pendingCalls[CallID] resolved.
//  3. If pendingCalls is empty, fires Manager.Resume(parent).
type ChildCompleted struct {
	CallID         string
	ChildSessionID string
	Result         DispatchResult
	CompletedAt    time.Time
}

func (ChildCompleted) isSessionMessage() {}

// FollowUpRouted is emitted by FollowUpRouter (spec-007) when a user
// message classifies onto a running child mission. The parent's actor
// forwards the FollowUpRouted into the targeted child's inbox; the
// child's actor appends a user_message event on its own session and
// fires runner.Run.
type FollowUpRouted struct {
	TargetChildSessionID string
	Text                 string
	OriginEventID        string // the parent's user_message event id
}

func (FollowUpRouted) isSessionMessage() {}

// CancelChild cascades a cancellation. Used by mission_cancel and
// orphan-sweeps. The receiving actor cancels its runner ctx, marks
// status=cancelled, emits ChildCompleted{Result.Error: "cancelled"}
// to its parent.
type CancelChild struct {
	CallID string
	Reason string
}

func (CancelChild) isSessionMessage() {}

// HITLQuestion is the Phase 5 escalation up-direction message. A
// child's ask_user tool emits this onto its parent's inbox. The
// parent decides on its next turn: answer (HITLAnswer back down) or
// escalate (own ask_user emits another HITLQuestion to grandparent).
type HITLQuestion struct {
	ChainID  string // shared id across the whole chain
	CallID   string // ask_user's function_call id on the asker
	Question string
	From     string // session id of the asker (for audit)
}

func (HITLQuestion) isSessionMessage() {}

// HITLAnswer is the Phase 5 down-direction message. The user's reply
// flows root→...→original-asker as HITLAnswer messages, with each
// hop optionally rewriting the answer (cleanup, context tagging).
type HITLAnswer struct {
	ChainID string
	Answer  string
	From    string
}

func (HITLAnswer) isSessionMessage() {}

// Resume is the sentinel that re-invokes runner.Run on the actor's
// session without appending any new event. Used after an approval
// flips, after a synthetic tool_result is appended on restart, etc.
type Resume struct {
	Reason string // free-form, audit only
}

func (Resume) isSessionMessage() {}

// ---------------------------------------------------------------------
// Dispatcher async API
// ---------------------------------------------------------------------

// AsyncDispatcher is the Phase-2 replacement for the synchronous
// Dispatcher.Run path. Existing Dispatcher type extends with this
// method; the legacy synchronous Run remains for tests during
// migration.
type AsyncDispatcher interface {
	// SpawnAsync opens a child session, registers a pending call on
	// the parent, and starts the child actor goroutine. Returns the
	// child session id immediately; blocks only on the parent's DB
	// row creation (~milliseconds).
	//
	// callID is the ADK function_call.id from tool.Context.FunctionCallID().
	// Used to pair the eventual function_response back to the parent's
	// pending tool_call.
	SpawnAsync(
		ctx context.Context,
		parentSessionID string,
		callID string,
		skill, role string,
		spec SubAgentSpec,
		task, notes string,
	) (childSessionID string, err error)
}

// DispatchResult is the payload that flows back to the parent as the
// function_response.response field. Identical fields to the existing
// pkg/agent/subagent.DispatchResult; named here so the design is
// self-contained.
type DispatchResult struct {
	Summary        string
	ChildSessionID string
	TurnsUsed      int
	Truncated      bool
	Error          string
}

// SubAgentSpec is a forward declaration; in real code it's
// skills.SubAgentSpec.
type SubAgentSpec struct {
	Description    string
	Intent         string
	Instructions   string
	MaxTurns       int
	SummaryMaxTok  int
	CanSpawn       bool
	MaxDepth       int
	RequiredSkills []string
}

// ---------------------------------------------------------------------
// Manager extensions
// ---------------------------------------------------------------------

// AsyncManager is the Phase-2 extension to *sessions.Manager. Existing
// Manager struct gains these methods; the interface here documents
// the shape.
type AsyncManager interface {
	// AppendFunctionResponse persists a tool_result event whose
	// metadata.call_id matches an outstanding long_running tool_call.
	// Invalidates the pendingCalls cache for the session. Idempotent
	// on duplicate (call_id, session_id) — second call is a no-op.
	AppendFunctionResponse(
		ctx context.Context,
		sessionID, callID string,
		payload DispatchResult,
	) error

	// Resume fires runner.Run on the named session if it has zero
	// pending calls. No-op if pending non-empty (parent stays paused
	// until siblings catch up). No-op if no actor goroutine exists
	// (used during restart bootstrap when goroutines come up later).
	Resume(ctx context.Context, sessionID string) error

	// PendingCalls returns the call_ids of unresolved long_running
	// tool_calls on the session. Derived from session_events via the
	// PendingCallsView query; cached 1s, invalidated on AppendEvent.
	PendingCalls(ctx context.Context, sessionID string) ([]string, error)
}

// ---------------------------------------------------------------------
// Pending calls view (DB-derived)
// ---------------------------------------------------------------------

// PendingCallsView is the SQL surface that derives pendingCalls from
// session_events. Lives in pkg/sessions/store. Implementations target
// both DuckDB (local) and Postgres (remote hub) via the same hugr
// query.
type PendingCallsView interface {
	// PendingCalls returns the call_ids of long_running tool_calls
	// on sessionID that lack a matching tool_result.
	PendingCalls(ctx context.Context, sessionID string) ([]string, error)
}

// ---------------------------------------------------------------------
// Restart classification
// ---------------------------------------------------------------------

// RestoreCase enumerates the disposition of an active session at
// boot time — see design.md "Case classification" table.
type RestoreCase int

const (
	RestoreUnknown RestoreCase = iota
	RestoreCaseA              // logically complete; mark + emit Completed
	RestoreCaseB              // idle; no spawn, await user_message
	RestoreCaseC              // sync tool interrupted; synth error + spawn
	RestoreCaseD              // long_running pending; respawn child
	RestoreCaseE              // long_running pending but child missing; synth fail
	RestoreCaseF              // stale (>24h); abandon
)

// RestoreClassifier inspects the last events of a session and picks
// the disposition. Pure function over (session row, recent events).
type RestoreClassifier interface {
	Classify(sessionID string, lastEvents []EventSummary, lastEventAt time.Time) RestoreCase
}

// EventSummary is the minimal projection RestoreClassifier needs.
type EventSummary struct {
	Seq          int64
	EventType    string
	ToolName     string
	CallID       string
	LongRunning  bool
	HasResult    bool // set when we already saw the matching tool_result
}

// ---------------------------------------------------------------------
// Process advisory lock
// ---------------------------------------------------------------------

// ProcessLock acquires the single-instance lock at boot. Implemented
// as an UPDATE on sessions.metadata.locked_by; stale locks (>5min)
// are taken over.
type ProcessLock interface {
	// Acquire returns nil if this process is now the active agent.
	// Returns ErrAnotherInstance if another non-stale instance holds
	// the lock.
	Acquire(ctx context.Context, pid int) error

	// Release clears the lock. Called from main on graceful shutdown.
	Release(ctx context.Context, pid int) error
}
