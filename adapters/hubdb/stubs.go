package hubdb

import (
	"context"
	"fmt"

	"github.com/hugr-lab/hugen/interfaces"
)

// notImplemented is returned by methods whose implementation is not
// currently in scope. Memory / Learning / Sessions-read extensions all
// landed in spec 005; the remaining stubs are cross-agent operations
// (UpsertAgentType, ListAgents) that belong on the hub side, and
// session notes/participants which land in spec 005 Phase 4 (US2).
func notImplemented(op string) error {
	return fmt.Errorf("hubdb: %s not implemented", op)
}

// UpsertAgentType is stubbed — agent_types rows are seeded during the
// initial migration (adapters/hubdb/migrate). Per-agent customisation
// lives in agents.config_override, not a runtime upsert.
func (h *hubDB) UpsertAgentType(ctx context.Context, at interfaces.AgentType) error {
	return notImplemented("UpsertAgentType")
}

// ListAgents is stubbed — an agent only reads and updates its own row.
// Cross-agent listing belongs on the hub side.
func (h *hubDB) ListAgents(ctx context.Context, typeID string) ([]interfaces.Agent, error) {
	return nil, notImplemented("ListAgents")
}

// ── Notes & Participants (spec 005 Phase 4 — User Story 2) ─────────

func (h *hubDB) AddNote(ctx context.Context, note interfaces.SessionNote) (string, error) {
	return "", notImplemented("AddNote")
}
func (h *hubDB) ListNotes(ctx context.Context, sessionID string) ([]interfaces.SessionNote, error) {
	return nil, notImplemented("ListNotes")
}
func (h *hubDB) DeleteNote(ctx context.Context, id string) error {
	return notImplemented("DeleteNote")
}
func (h *hubDB) DeleteSessionNotes(ctx context.Context, sessionID string) (int, error) {
	return 0, notImplemented("DeleteSessionNotes")
}
func (h *hubDB) AddParticipant(ctx context.Context, p interfaces.SessionParticipant) error {
	return notImplemented("AddParticipant")
}
func (h *hubDB) RemoveParticipant(ctx context.Context, sessionID, userID string) error {
	return notImplemented("RemoveParticipant")
}
func (h *hubDB) ListParticipants(ctx context.Context, sessionID string) ([]interfaces.SessionParticipant, error) {
	return nil, notImplemented("ListParticipants")
}
