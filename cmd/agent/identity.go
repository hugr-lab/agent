package main

import (
	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/identity/hub"
	"github.com/hugr-lab/hugen/pkg/identity/local"
	hugenruntime "github.com/hugr-lab/hugen/pkg/runtime"
	qetypes "github.com/hugr-lab/query-engine/types"
)

// buildIdentity is Phase 5: chooses the right identity.Source for
// the active mode.
//
//   - Remote mode (boot.Remote()): hub.Source with the remote
//     querier; agent ID resolves lazily via WhoAmI on first Agent()
//     call.
//   - Pure local (no remote querier): local.Source backed by
//     config.yaml; WhoAmI returns a static "local" stub.
//   - Hybrid (local YAML + reachable hub): local.NewWithHub —
//     agent identity comes from YAML, but WhoAmI / Permission
//     delegate to the hub for real principal data.
//
// configPath is the operator-configured path to the local YAML
// (typically "config.yaml").
func buildIdentity(boot *hugenruntime.BootstrapConfig, remote qetypes.Querier, configPath string) identity.Source {
	switch {
	case boot.Remote():
		// Remote mode: hub is the source of truth for both identity
		// and config. configPath is irrelevant.
		return hub.New(remote)
	case remote == nil:
		// Pure local: no hub reachable, identity comes from yaml only.
		return local.New(configPath)
	default:
		// Hybrid: yaml drives identity, hub answers WhoAmI/Permission.
		return local.NewWithHub(hub.New(remote), configPath)
	}
}
