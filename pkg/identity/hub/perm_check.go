package hub

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/store/queries"
)

// Permission queries function.core.auth.check_access_info for
// (type_name=section, field=name). The shape mirrors the hub auth
// API exactly: enabled is the gate, data and filter carry the
// row-level policy payloads.
func (s *Source) Permission(ctx context.Context, section, name string) (identity.Permission, error) {
	return queries.RunQuery[identity.Permission](ctx, s.qe, `query ($section: String!, $name: String!) {
		function { core { auth { check_access_info(type_name: $section, field: $name){
			enabled
			data
			filter
		} } } }
	}`, map[string]any{
		"section": section,
		"name":    name,
	}, "function.core.auth.check_access_info")
}
