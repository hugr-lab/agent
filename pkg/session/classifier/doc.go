// Package classifier maps ADK session events to hub.db.session_events
// rows. Runs as a single goroutine drained from a buffered channel
// fed by Session.IngestADKEvent.
//
// The classifier is intentionally separate from the Session struct:
//   - Session is ADK-agnostic; it knows nothing about the shape of
//     adksession.Event.
//   - Classification has moving parts (partials, tool call/result,
//     truncation handling) that deserve their own test surface.
//   - Writing to hub.db can stall on I/O; keeping that goroutine off
//     the user-turn critical path is a spec invariant (FR-001, SC-009).
//
// Drop-on-full: if the buffered channel is saturated the producer
// drops the event with a WARN log and a bumped dropped counter. The
// transcript is telemetry, not correctness; blocking the LLM turn to
// preserve a single event is the wrong trade.
package classifier
