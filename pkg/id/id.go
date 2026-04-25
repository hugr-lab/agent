// Package id generates synthetic sortable identifiers for hubdb entities.
//
// Format: {prefix}_{agentShort}_{unixTimestamp}_{3-byte-hex}
//
//	id.New(PrefixMemory, "ag01") => "mem_ag01_1713345600_a3f8b2"
//
// Properties:
//   - Time-sortable: string ORDER BY = chronological order (within prefix+agent)
//   - Prefix-identifiable: entity type visible in logs
//   - Agent-scoped: cross-agent references immediately distinguishable
//   - Collision-safe: 3-byte random suffix (16M per-second per-agent)
package id

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Prefix constants for entity types.
const (
	PrefixAgent      = "agt"
	PrefixMemory     = "mem"
	PrefixHypothesis = "hyp"
	PrefixNote       = "note"
	PrefixReview     = "rev"
	PrefixEvent      = "evt"
	PrefixArtifact   = "art"
)

// ErrInvalid indicates a malformed ID string.
var ErrInvalid = errors.New("id: invalid format")

// New returns a synthetic sortable ID for the given entity prefix and agent short ID.
// agentShort should be a short alias (typically 4 chars) identifying the owning agent.
func New(prefix, agentShort string) string {
	return newAt(prefix, agentShort, time.Now())
}

func newAt(prefix, agentShort string, ts time.Time) string {
	buf := make([]byte, 3)
	_, _ = rand.Read(buf)
	return fmt.Sprintf("%s_%s_%d_%s", prefix, agentShort, ts.Unix(), hex.EncodeToString(buf))
}

// Parsed holds the components of a decoded ID.
type Parsed struct {
	Prefix     string
	AgentShort string
	Timestamp  time.Time
	Random     string
}

// Parse decomposes an ID into its parts. Returns ErrInvalid for malformed input.
func Parse(id string) (Parsed, error) {
	parts := strings.SplitN(id, "_", 4)
	if len(parts) != 4 || parts[0] == "" || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		return Parsed{}, ErrInvalid
	}
	unix, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return Parsed{}, ErrInvalid
	}
	return Parsed{
		Prefix:     parts[0],
		AgentShort: parts[1],
		Timestamp:  time.Unix(unix, 0).UTC(),
		Random:     parts[3],
	}, nil
}
