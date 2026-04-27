// Package local is the YAML-driven identity.Source used in fully
// local mode (no hub) and as the file-overlay half of the hybrid
// local-with-hub combination.
//
// In pure-local mode there is no auth principal: WhoAmI returns a
// stub user with the literal "local" id. In local-with-hub mode the
// embedded hub Source supplies the real principal + permissions but
// agent config still comes from the YAML file.
package local

import (
	"github.com/hugr-lab/hugen/pkg/identity/hub"
)

// Source resolves the running agent against a local YAML file.
// When hub is non-nil, WhoAmI / Permission delegate to it — the
// "local with hub" hybrid in cmd/agent.
type Source struct {
	hub        *hub.Source
	configPath string
}

// New builds a pure-local Source: no hub, all calls answered from
// configPath / static stub.
func New(configPath string) *Source {
	return &Source{configPath: configPath}
}

// NewWithHub builds the hybrid Source: Agent() reads YAML, but
// WhoAmI() / Permission() delegate to the embedded hub Source.
func NewWithHub(hub *hub.Source, configPath string) *Source {
	return &Source{hub: hub, configPath: configPath}
}
