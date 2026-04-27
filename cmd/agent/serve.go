package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// serve binds a listener for each server, fires prompt-login hooks
// once every listener is live, and blocks until ctx is cancelled or
// one of the servers errors. On ctx cancel it triggers graceful
// shutdown on every server concurrently.
//
// Returns the first non-trivial serve error; http.ErrServerClosed is
// treated as a clean exit.
func serve(ctx context.Context, servers []*http.Server, prompts []func(), logger *slog.Logger) error {
	if len(servers) == 0 {
		return fmt.Errorf("serve: no servers")
	}

	type result struct {
		err  error
		addr string
	}
	results := make(chan result, len(servers))
	listeners := make([]net.Listener, 0, len(servers))

	for _, srv := range servers {
		ln, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			for _, l := range listeners {
				_ = l.Close()
			}
			return fmt.Errorf("listen %s: %w", srv.Addr, err)
		}
		listeners = append(listeners, ln)
		s := srv
		go func() {
			err := s.Serve(ln)
			results <- result{err: err, addr: s.Addr}
		}()
	}
	for _, p := range prompts {
		go p()
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		logger.Info("shutting down: draining requests")
		for _, s := range servers {
			_ = s.Shutdown(shutdownCtx)
		}
	}()

	var firstErr error
	for range servers {
		r := <-results
		if r.err != nil && r.err != http.ErrServerClosed && firstErr == nil {
			firstErr = fmt.Errorf("serve %s: %w", r.addr, r.err)
		}
	}
	return firstErr
}

// corsMiddleware allows the DevUI listener's SPA (served from the same
// origin) and loopback clients to call the /api endpoints without a
// preflight rejection. For non-localhost origins it restricts to the
// exact DevUI base URL.
func corsMiddleware(baseURL string, next http.Handler) http.Handler {
	origin := baseURL
	if strings.Contains(baseURL, "localhost") || strings.Contains(baseURL, "127.0.0.1") {
		origin = "*"
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
