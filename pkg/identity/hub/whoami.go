package hub

import (
	"context"
	"errors"
	"fmt"

	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/store/queries"
)

// WhoAmI executes
//
//	query { function { core { auth { me { user_id user_name role } } } } }
//
// and returns the decoded subject. Errors include the usual
// transport / GraphQL failures; an empty user_id also returns an
// error to prevent us from keying hub.db with "".
func (s *Source) WhoAmI(ctx context.Context) (identity.WhoAmI, error) {
	const gql = `query {
		function { core { auth { me {
			user_id
			user_name
			role
		} } } }
	}`

	me, err := queries.RunQuery[identity.WhoAmI](ctx, s.qe, gql, nil, "function.core.auth.me")
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return identity.WhoAmI{}, fmt.Errorf("identity hub: hugr auth.me returned empty payload")
		}
		return identity.WhoAmI{}, fmt.Errorf("identity hub: resolve whoami: %w", err)
	}
	if me.UserID == "" {
		return identity.WhoAmI{}, fmt.Errorf("identity hub: hugr returned empty user_id")
	}
	return me, nil
}
