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
	querier    types.Querier
	agentID    string
	agentShort string
	dimension  int
	logger     *slog.Logger
}

// New constructs a HubDB backed by the given querier.
func New(querier types.Querier, agentID, agentShort string, dimension int, logger *slog.Logger) (interfaces.HubDB, error) {
	if querier == nil {
		return nil, fmt.Errorf("hubdb: nil querier")
	}
	if agentID == "" {
		return nil, fmt.Errorf("hubdb: agentID required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &hubDB{
		querier:    querier,
		agentID:    agentID,
		agentShort: agentShort,
		dimension:  dimension,
		logger:     logger,
	}, nil
}

func (h *hubDB) AgentID() string { return h.agentID }
func (h *hubDB) Close() error    { return nil }
