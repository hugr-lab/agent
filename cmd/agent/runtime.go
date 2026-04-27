package main

import (
	"context"
	"log/slog"
	"net/http"

	hugenruntime "github.com/hugr-lab/hugen/pkg/runtime"

	qe "github.com/hugr-lab/query-engine"
	"github.com/hugr-lab/query-engine/client"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/models"
	qetypes "github.com/hugr-lab/query-engine/types"
)

// agentRuntime bundles all long-lived resources built at startup.
// Embeds *hugenruntime.Runtime for direct access to Agent / Sessions /
// Tools / Skills / Engine; shutdown delegates to Runtime.Close.
type agentRuntime struct {
	*hugenruntime.Runtime
	hugrClient *client.Client
}

func (r *agentRuntime) close(logger *slog.Logger) {
	if r == nil || r.Runtime == nil {
		return
	}
	logger.Info("shutting down: closing runtime")
	r.Close()
}

// runtimeInputs is the bag of pre-built externals Phase 9 wires into
// hugenruntime.Build. Each field comes from one of the earlier
// numbered phases — bundling them keeps buildRuntime a thin wrapper
// instead of leaking ~10 positional parameters.
type runtimeInputs struct {
	LocalQuerier  qetypes.Querier
	RemoteQuerier qetypes.Querier
	LocalEngine   *qe.Service
	Router        *models.Router
	AuthService   *auth.Service
	HugrClient    *client.Client
}

// buildRuntime is Phase 9: thin wrapper around hugenruntime.Build.
// All heavy lifting (engine, router, hub clients, queriers) is done
// in earlier phases — buildRuntime just stitches them into the
// hugenruntime Options shape and registers any caller-owned closers.
func buildRuntime(ctx context.Context, cfg *hugenruntime.RuntimeConfig, in runtimeInputs, logger *slog.Logger) (*agentRuntime, error) {
	opts := hugenruntime.Options{
		BaseTransport: http.DefaultTransport,
		LocalQuerier:  in.LocalQuerier,
		RemoteQuerier: in.RemoteQuerier,
		LocalEngine:   in.LocalEngine,
		Router:        in.Router,
	}
	if in.AuthService != nil {
		opts.AuthStores = in.AuthService.TokenStores()
	}
	if in.HugrClient != nil {
		opts.ExtraClose = append(opts.ExtraClose, in.HugrClient.CloseSubscriptions)
	}

	rt, err := hugenruntime.Build(ctx, cfg, logger, opts)
	if err != nil {
		if in.LocalEngine != nil {
			_ = in.LocalEngine.Close()
		}
		return nil, err
	}
	return &agentRuntime{
		Runtime:    rt,
		hugrClient: in.HugrClient,
	}, nil
}
