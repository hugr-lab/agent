package hubdb

import (
	"fmt"
	"log/slog"

	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/interfaces"
)

// hubDB implements interfaces.HubDB on top of a types.Querier. The same
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

// Options bundles HubDB construction parameters.
type Options struct {
	AgentID        string
	AgentShort     string
	Dimension      int
	EmbeddingModel string
	Logger         *slog.Logger
}

// New constructs a HubDB backed by the given querier.
func New(querier types.Querier, cfg Options) (interfaces.HubDB, error) {
	if querier == nil {
		return nil, fmt.Errorf("hubdb: nil querier")
	}
	if cfg.AgentID == "" {
		return nil, fmt.Errorf("hubdb: AgentID required")
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
