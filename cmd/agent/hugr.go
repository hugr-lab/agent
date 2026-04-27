package main

import (
	"log/slog"
	"net/http"

	"github.com/hugr-lab/hugen/pkg/auth"
	hugenruntime "github.com/hugr-lab/hugen/pkg/runtime"
	"github.com/hugr-lab/query-engine/client"
)

// connectRemote is Phase 4: builds the remote hugr GraphQL client
// using a token-bearing transport derived from the auth Service.
//
// Returns nil when boot.Hugr.URL is unset (purely local mode) or when
// the auth Service does not expose a "hugr" token store (auth was
// skipped because no HUGR_URL was configured at boot).
func connectRemote(boot *hugenruntime.BootstrapConfig, svc *auth.Service, logger *slog.Logger) *client.Client {
	if boot.Hugr.URL == "" {
		return nil
	}
	ts, ok := svc.TokenStore("hugr")
	if !ok {
		logger.Warn("connectRemote: no hugr token store; running without remote client")
		return nil
	}
	return client.NewClient(
		boot.Hugr.URL+"/ipc",
		client.WithTransport(auth.Transport(ts, http.DefaultTransport)),
	)
}
