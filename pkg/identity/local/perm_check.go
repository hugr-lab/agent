package local

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/identity"
)

// Permission grants everything in pure-local mode (no auth subject
// to check) and delegates to the hub in hybrid mode.
func (s *Source) Permission(ctx context.Context, section, name string) (identity.Permission, error) {
	if s.hub == nil {
		return identity.Permission{Enabled: true}, nil
	}
	return s.hub.Permission(ctx, section, name)
}
