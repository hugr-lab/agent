package hubdb

import (
	"context"
	"fmt"
	"time"

	"github.com/hugr-lab/hugen/interfaces"
)

// notImplemented is returned by methods whose implementation lives in future
// specs (003b Memory/Learning/Sessions). AgentRegistry (US2) and Embeddings
// (US4) replace these stubs with real GraphQL calls.
func notImplemented(op string) error {
	return fmt.Errorf("hubdb: %s not implemented in 004", op)
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

// ── Memory (filled in by 003b) ──────────────────────────────

func (h *hubDB) Search(ctx context.Context, query string, embedding []float32, opts interfaces.SearchOpts) ([]interfaces.SearchResult, error) {
	return nil, notImplemented("Search")
}
func (h *hubDB) Get(ctx context.Context, id string) (*interfaces.SearchResult, error) {
	return nil, notImplemented("Get")
}
func (h *hubDB) GetLinked(ctx context.Context, id string, depth int) ([]interfaces.SearchResult, error) {
	return nil, notImplemented("GetLinked")
}
func (h *hubDB) Store(ctx context.Context, item interfaces.MemoryItem, tags []string, links []interfaces.MemoryLink) (string, error) {
	return "", notImplemented("Store")
}
func (h *hubDB) Reinforce(ctx context.Context, id string, scoreBonus float64, extraTags []string, extraLinks []interfaces.MemoryLink) error {
	return notImplemented("Reinforce")
}
func (h *hubDB) Supersede(ctx context.Context, oldID string, newItem interfaces.MemoryItem, tags []string, links []interfaces.MemoryLink) (string, error) {
	return "", notImplemented("Supersede")
}
func (h *hubDB) Delete(ctx context.Context, id string) error        { return notImplemented("Delete") }
func (h *hubDB) DeleteExpired(ctx context.Context) (int, error)     { return 0, notImplemented("DeleteExpired") }
func (h *hubDB) AddTags(ctx context.Context, id string, tags []string) error {
	return notImplemented("AddTags")
}
func (h *hubDB) RemoveTags(ctx context.Context, id string, tags []string) error {
	return notImplemented("RemoveTags")
}
func (h *hubDB) AddLink(ctx context.Context, link interfaces.MemoryLink) error {
	return notImplemented("AddLink")
}
func (h *hubDB) RemoveLink(ctx context.Context, sourceID, targetID string) error {
	return notImplemented("RemoveLink")
}
func (h *hubDB) Stats(ctx context.Context) (interfaces.MemoryStats, error) {
	return interfaces.MemoryStats{}, notImplemented("Stats")
}
func (h *hubDB) Hint(ctx context.Context, query string, embedding []float32) (string, error) {
	return "", notImplemented("Hint")
}

// ── Learning (filled in by 003b) ────────────────────────────

func (h *hubDB) CreateHypothesis(ctx context.Context, hyp interfaces.Hypothesis) (string, error) {
	return "", notImplemented("CreateHypothesis")
}
func (h *hubDB) ListPendingHypotheses(ctx context.Context, priority string, limit int) ([]interfaces.Hypothesis, error) {
	return nil, notImplemented("ListPendingHypotheses")
}
func (h *hubDB) MarkHypothesisChecking(ctx context.Context, id string) error {
	return notImplemented("MarkHypothesisChecking")
}
func (h *hubDB) ConfirmHypothesis(ctx context.Context, id string, evidence, factID string) error {
	return notImplemented("ConfirmHypothesis")
}
func (h *hubDB) RejectHypothesis(ctx context.Context, id string, evidence string) error {
	return notImplemented("RejectHypothesis")
}
func (h *hubDB) DeferHypothesis(ctx context.Context, id string) error {
	return notImplemented("DeferHypothesis")
}
func (h *hubDB) ExpireOldHypotheses(ctx context.Context, maxAge time.Duration) (int, error) {
	return 0, notImplemented("ExpireOldHypotheses")
}
func (h *hubDB) CreateReview(ctx context.Context, r interfaces.SessionReview) (string, error) {
	return "", notImplemented("CreateReview")
}
func (h *hubDB) GetReview(ctx context.Context, sessionID string) (*interfaces.SessionReview, error) {
	return nil, notImplemented("GetReview")
}
func (h *hubDB) ListPendingReviews(ctx context.Context, limit int) ([]interfaces.SessionReview, error) {
	return nil, notImplemented("ListPendingReviews")
}
func (h *hubDB) CompleteReview(ctx context.Context, id string, result interfaces.ReviewResult) error {
	return notImplemented("CompleteReview")
}
func (h *hubDB) FailReview(ctx context.Context, id string, errMsg string) error {
	return notImplemented("FailReview")
}
func (h *hubDB) Log(ctx context.Context, entry interfaces.MemoryLogEntry) error {
	return notImplemented("Log")
}
func (h *hubDB) GetLog(ctx context.Context, memoryItemID string, limit int) ([]interfaces.MemoryLogEntry, error) {
	return nil, notImplemented("GetLog")
}

// ── Sessions (filled in by 003b) ────────────────────────────

func (h *hubDB) CreateSession(ctx context.Context, s interfaces.Session) (string, error) {
	return "", notImplemented("CreateSession")
}
func (h *hubDB) GetSession(ctx context.Context, id string) (*interfaces.Session, error) {
	return nil, notImplemented("GetSession")
}
func (h *hubDB) ListActiveSessions(ctx context.Context) ([]interfaces.Session, error) {
	return nil, notImplemented("ListActiveSessions")
}
func (h *hubDB) ListChildSessions(ctx context.Context, parentSessionID string) ([]interfaces.Session, error) {
	return nil, notImplemented("ListChildSessions")
}
func (h *hubDB) UpdateSessionStatus(ctx context.Context, id, status string) error {
	return notImplemented("UpdateSessionStatus")
}
func (h *hubDB) AppendEvent(ctx context.Context, event interfaces.SessionEvent) (string, error) {
	return "", notImplemented("AppendEvent")
}
func (h *hubDB) GetEvents(ctx context.Context, sessionID string) ([]interfaces.SessionEvent, error) {
	return nil, notImplemented("GetEvents")
}
func (h *hubDB) GetEventsFull(ctx context.Context, sessionID string) ([]interfaces.SessionEventFull, error) {
	return nil, notImplemented("GetEventsFull")
}
func (h *hubDB) CountToolCalls(ctx context.Context, sessionID string) (int, error) {
	return 0, notImplemented("CountToolCalls")
}
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

