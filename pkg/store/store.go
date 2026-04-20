// Package store is the agent's data access layer. It exposes a Store
// interface aggregating Memory, Learning, Sessions, AgentRegistry and
// Embeddings sub-interfaces, plus a GraphQL-backed implementation that
// works against any types.Querier (embedded hugr engine or remote
// client).
//
// All operations are scoped to one agent identity, passed at
// construction. Data types (MemoryItem, Hypothesis, SessionRecord, …)
// live in types.go; event-type constants and *Meta payloads live in
// events.go.
package store

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/hugr-lab/query-engine/types"
)

// DB is the aggregate data-access interface. Implementations must
// keep every call safe for concurrent use — multiple services hit the
// store from different goroutines (classifier, scheduler workers,
// tool handlers).
type DB interface {
	AgentID() string

	AgentRegistry
	Memory
	Learning
	Sessions
	Embeddings

	Close() error
}

// AgentRegistry manages agent type configurations and running instances.
type AgentRegistry interface {
	GetAgentType(ctx context.Context, typeID string) (*AgentType, error)
	UpsertAgentType(ctx context.Context, at AgentType) error
	GetAgent(ctx context.Context, id string) (*Agent, error)
	RegisterAgent(ctx context.Context, a Agent) error
	UpdateAgentActivity(ctx context.Context, id string) error
	ListAgents(ctx context.Context, typeID string) ([]Agent, error)
}

// Memory manages persistent facts with tags, links, and vector search.
// APPEND-ONLY: Reinforce/Supersede = DELETE + INSERT.
type Memory interface {
	Search(ctx context.Context, query string, embedding []float32, opts SearchOpts) ([]SearchResult, error)
	Get(ctx context.Context, id string) (*SearchResult, error)
	GetLinked(ctx context.Context, id string, depth int) ([]SearchResult, error)

	Store(ctx context.Context, item MemoryItem, tags []string, links []MemoryLink) (string, error)
	Reinforce(ctx context.Context, id string, scoreBonus float64, extraTags []string, extraLinks []MemoryLink) error
	Supersede(ctx context.Context, oldID string, newItem MemoryItem, tags []string, links []MemoryLink) (string, error)
	Delete(ctx context.Context, id string) error
	DeleteExpired(ctx context.Context) (int, error)

	AddTags(ctx context.Context, memoryItemID string, tags []string) error
	RemoveTags(ctx context.Context, memoryItemID string, tags []string) error

	AddLink(ctx context.Context, link MemoryLink) error
	RemoveLink(ctx context.Context, sourceID, targetID string) error

	Stats(ctx context.Context) (MemoryStats, error)
	Hint(ctx context.Context, query string, embedding []float32) (string, error)
}

// Learning manages hypotheses, reviews, and the memory audit log.
type Learning interface {
	CreateHypothesis(ctx context.Context, h Hypothesis) (string, error)
	ListPendingHypotheses(ctx context.Context, priority string, limit int) ([]Hypothesis, error)
	MarkHypothesisChecking(ctx context.Context, id string) error
	ConfirmHypothesis(ctx context.Context, id string, evidence, factID string) error
	RejectHypothesis(ctx context.Context, id string, evidence string) error
	DeferHypothesis(ctx context.Context, id string) error
	ExpireOldHypotheses(ctx context.Context, maxAge time.Duration) (int, error)

	CreateReview(ctx context.Context, review SessionReview) (string, error)
	GetReview(ctx context.Context, sessionID string) (*SessionReview, error)
	ListPendingReviews(ctx context.Context, limit int) ([]SessionReview, error)
	CompleteReview(ctx context.Context, id string, result ReviewResult) error
	FailReview(ctx context.Context, id string, errMsg string) error

	Log(ctx context.Context, entry MemoryLogEntry) error
	GetLog(ctx context.Context, memoryItemID string, limit int) ([]MemoryLogEntry, error)
}

// Sessions manages conversation sessions, events, notes, and participants.
type Sessions interface {
	CreateSession(ctx context.Context, s SessionRecord) (string, error)
	GetSession(ctx context.Context, id string) (*SessionRecord, error)
	ListActiveSessions(ctx context.Context) ([]SessionRecord, error)
	ListChildSessions(ctx context.Context, parentSessionID string) ([]SessionRecord, error)
	UpdateSessionStatus(ctx context.Context, id, status string) error

	AppendEvent(ctx context.Context, event SessionEvent) (string, error)
	GetEvents(ctx context.Context, sessionID string) ([]SessionEvent, error)
	GetEventsFull(ctx context.Context, sessionID string) ([]SessionEventFull, error)
	CountToolCalls(ctx context.Context, sessionID string) (int, error)

	AddNote(ctx context.Context, note SessionNote) (string, error)
	ListNotes(ctx context.Context, sessionID string) ([]SessionNote, error)
	DeleteNote(ctx context.Context, id string) error
	DeleteSessionNotes(ctx context.Context, sessionID string) (int, error)

	AddParticipant(ctx context.Context, p SessionParticipant) error
	RemoveParticipant(ctx context.Context, sessionID, userID string) error
	ListParticipants(ctx context.Context, sessionID string) ([]SessionParticipant, error)
}

// Embeddings wraps the embedding model registered in the engine.
type Embeddings interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
	Dimension() int
	Available() bool
}

// hubDB implements Store on top of a types.Querier. The same
// implementation works for the embedded hugr.Service and the remote
// client.Client.
type hubDB struct {
	querier        types.Querier
	agentID        string
	agentShort     string
	dimension      int
	embeddingModel string
	logger         *slog.Logger
}

// Options bundles DB construction parameters.
type Options struct {
	AgentID        string
	AgentShort     string
	Dimension      int
	EmbeddingModel string
	Logger         *slog.Logger
}

// New constructs a DB backed by the given querier.
func New(querier types.Querier, cfg Options) (DB, error) {
	if querier == nil {
		return nil, fmt.Errorf("store: nil querier")
	}
	if cfg.AgentID == "" {
		return nil, fmt.Errorf("store: AgentID required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &hubDB{
		querier:        querier,
		agentID:        cfg.AgentID,
		agentShort:     cfg.AgentShort,
		dimension:      cfg.Dimension,
		embeddingModel: cfg.EmbeddingModel,
		logger:         cfg.Logger,
	}, nil
}

func (h *hubDB) AgentID() string { return h.agentID }
func (h *hubDB) Close() error    { return nil }
