// Package auth provides token management for the hugr-agent.
//
// TokenStore is the central interface — it returns a valid Bearer access token
// for authenticating with Hugr. Implementations handle token refresh
// transparently.
//
// Selection logic:
//
//	HUGR_ACCESS_TOKEN + HUGR_TOKEN_URL → RemoteStore
//	otherwise                          → OIDCStore (device flow via {HUGR_URL}/auth/config)
package auth

import "context"

// TokenStore provides access tokens for Hugr API authentication.
type TokenStore interface {
	// Token returns a valid access token. Implementations must handle
	// refresh/exchange internally — callers always get a ready-to-use token.
	Token(ctx context.Context) (string, error)
}
