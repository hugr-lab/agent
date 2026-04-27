package local

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/identity"
)

// WhoAmI returns the authenticated subject. In pure-local mode
// there is no auth principal — we return a stub. In hybrid mode
// the hub Source answers, since it has access to the real bearer
// token via the hugr transport.
func (s *Source) WhoAmI(ctx context.Context) (identity.WhoAmI, error) {
	if s.hub == nil {
		return identity.WhoAmI{
			UserID:   "local",
			UserName: "local",
			Role:     "local",
		}, nil
	}
	return s.hub.WhoAmI(ctx)
}
