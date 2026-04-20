package sessions

import "context"

type ctxKey struct{}

// WithID returns ctx carrying sessionID, so deep callers (tools, helpers)
// can look up the current session without threading the ID explicitly.
func WithID(ctx context.Context, sessionID string) context.Context {
	if sessionID == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, sessionID)
}

// IDFromContext returns the session ID carried by ctx, or "" if none.
func IDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(ctxKey{}).(string)
	return v
}
