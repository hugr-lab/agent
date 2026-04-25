package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/pkg/store/queries"
)

// GrantRecord mirrors an artifact_grants row.
type GrantRecord struct {
	ArtifactID string
	AgentID    string
	SessionID  string
	GrantedBy  string
	GrantedAt  time.Time
}

// AddGrant inserts a row into hub.db.agent.artifact_grants.
// Idempotent on PK collision: re-issuing the same grant returns nil.
func (c *Client) AddGrant(ctx context.Context, g GrantRecord) error {
	if g.ArtifactID == "" {
		return fmt.Errorf("artifacts/store: AddGrant requires ArtifactID")
	}
	if g.SessionID == "" {
		return fmt.Errorf("artifacts/store: AddGrant requires SessionID")
	}
	if g.GrantedBy == "" {
		return fmt.Errorf("artifacts/store: AddGrant requires GrantedBy")
	}
	if g.AgentID == "" {
		g.AgentID = c.agentID
	}
	data := map[string]any{
		"artifact_id": g.ArtifactID,
		"agent_id":    g.AgentID,
		"session_id":  g.SessionID,
		"granted_by":  g.GrantedBy,
	}
	err := queries.RunMutation(ctx, c.querier,
		`mutation ($data: hub_db_artifact_grants_mut_input_data!) {
			hub { db { agent {
				insert_artifact_grants(data: $data) { artifact_id }
			}}}
		}`,
		map[string]any{"data": data},
	)
	if err != nil {
		// Insert duplicate-key on a composite PK is the legitimate
		// "grant already present" case — treat it as success.
		if isDuplicateKey(err) {
			return nil
		}
		return fmt.Errorf("artifacts/store: add grant: %w", err)
	}
	return nil
}

// RemoveGrantsByArtifact deletes every grant pointing at the given
// artifact id. Used by Manager.Remove + Manager.Cleanup before
// deleting the artifact row itself, since the schema has no FK to
// cascade.
func (c *Client) RemoveGrantsByArtifact(ctx context.Context, artifactID string) error {
	if artifactID == "" {
		return fmt.Errorf("artifacts/store: RemoveGrantsByArtifact requires artifactID")
	}
	return queries.RunMutation(ctx, c.querier,
		`mutation ($aid: String!) {
			hub { db { agent {
				delete_artifact_grants(filter: {artifact_id: {eq: $aid}}) { affected_rows }
			}}}
		}`,
		map[string]any{"aid": artifactID},
	)
}

// grantRow mirrors a flat artifact_grants row.
type grantRow struct {
	ArtifactID string    `json:"artifact_id"`
	AgentID    string    `json:"agent_id"`
	SessionID  string    `json:"session_id"`
	GrantedBy  string    `json:"granted_by"`
	GrantedAt  time.Time `json:"granted_at"`
}

func (r grantRow) toRecord() GrantRecord {
	return GrantRecord(r)
}

// ListGrantsForSession returns every grant whose target is the given
// (agent, session). Used by tests + the manager's Remove path
// (revoke before delete).
func (c *Client) ListGrantsForSession(ctx context.Context, agentID, sessionID string) ([]GrantRecord, error) {
	if agentID == "" || sessionID == "" {
		return nil, fmt.Errorf("artifacts/store: ListGrantsForSession requires agentID + sessionID")
	}
	rows, err := queries.RunQuery[[]grantRow](ctx, c.querier,
		`query ($agent: String!, $sid: String!) {
			hub { db { agent {
				artifact_grants(filter: {agent_id: {eq: $agent}, session_id: {eq: $sid}}) {
					artifact_id agent_id session_id granted_by granted_at
				}
			}}}
		}`,
		map[string]any{"agent": agentID, "sid": sessionID},
		"hub.db.agent.artifact_grants",
	)
	if err != nil {
		if errors.Is(err, types.ErrNoData) || errors.Is(err, types.ErrWrongDataPath) {
			return nil, nil
		}
		return nil, fmt.Errorf("artifacts/store: list grants: %w", err)
	}
	out := make([]GrantRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.toRecord())
	}
	return out, nil
}

// isDuplicateKey reports whether err is a PK / unique constraint
// violation. DuckDB and Postgres surface different messages; we
// match on substring rather than driver-specific error codes (the
// query-engine wraps both behind types.Querier without exposing the
// underlying driver).
func isDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, hint := range []string{
		"duplicate key",
		"unique constraint",
		"PRIMARY KEY constraint",
		"violates primary key",
	} {
		if containsFold(msg, hint) {
			return true
		}
	}
	return false
}

// containsFold reports whether s contains substr (case-insensitive
// ASCII match — sufficient for engine error strings).
func containsFold(s, substr string) bool {
	if len(substr) == 0 {
		return true
	}
	if len(substr) > len(s) {
		return false
	}
	for i := 0; i+len(substr) <= len(s); i++ {
		match := true
		for j := 0; j < len(substr); j++ {
			a, b := s[i+j], substr[j]
			if a >= 'A' && a <= 'Z' {
				a += 'a' - 'A'
			}
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
