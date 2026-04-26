package search

import "errors"

// Sentinel errors. Callers should use errors.Is to match.
var (
	// ErrTurnRejectsQuery is returned by session_context when scope
	// is "turn" and the caller passes a `query` argument. Turn is a
	// tail, not a search.
	ErrTurnRejectsQuery = errors.New("search: scope=turn does not accept query (use scope=mission)")

	// ErrScopeForbidden is returned when a sub-agent calls
	// session_context with scope ∈ {session, user}. The error
	// message includes a hint pointing at ask_coordinator as the
	// alternative path.
	ErrScopeForbidden = errors.New("search: scope is coordinator-only; use ask_coordinator to reach past data")

	// ErrInvalidScope is returned when scope is outside the recognised enum.
	ErrInvalidScope = errors.New("search: invalid scope")

	// ErrInvalidDateRange is returned when date_from > date_to.
	ErrInvalidDateRange = errors.New("search: date_from > date_to")

	// ErrUnparseableHalfLife is returned when half_life is not a
	// valid time.Duration-compatible string.
	ErrUnparseableHalfLife = errors.New("search: unparseable half_life (want '1h', '24h', '168h', etc.)")

	// ErrUnknownSession is returned by session_events when the
	// requested session_id does not resolve.
	ErrUnknownSession = errors.New("search: unknown session")

	// ErrSessionNotInScope is returned by session_events when the
	// caller (sub-agent or coord) requests a session outside its
	// allowed read scope.
	ErrSessionNotInScope = errors.New("search: session not in caller's scope")
)
