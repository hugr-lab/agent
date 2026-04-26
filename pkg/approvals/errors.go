package approvals

import "errors"

// Sentinel errors. Callers should use errors.Is to match.
var (
	// ErrApprovalNotFound is returned by Manager.Get / Respond when
	// the requested approvals row does not exist.
	ErrApprovalNotFound = errors.New("approvals: approval not found")

	// ErrAlreadyResolved is returned by Manager.Respond when the row
	// has already left the Pending status (whether by user response
	// or sweeper expiration).
	ErrAlreadyResolved = errors.New("approvals: already resolved")

	// ErrApprovalExpired is a more specific form of ErrAlreadyResolved
	// surfaced when the prior terminal state was StatusExpired. Lets
	// the coordinator render "approval timed out — want me to retry?"
	// instead of generic "already resolved".
	ErrApprovalExpired = errors.New("approvals: already expired")

	// ErrInvalidDecision is returned when RespondPayload.Decision is
	// outside the recognised enum.
	ErrInvalidDecision = errors.New("approvals: invalid decision")

	// ErrModifiedArgsMissing is returned by Respond when Decision ==
	// DecisionModify but ModifiedArgs is empty.
	ErrModifiedArgsMissing = errors.New("approvals: decision=modify requires modified_args")

	// ErrAnswerMissing is returned by Respond when Decision ==
	// DecisionAnswer but Answer is empty.
	ErrAnswerMissing = errors.New("approvals: decision=answer requires non-empty answer")

	// ErrAnswerOnNonAsk is returned by Respond when Decision ==
	// DecisionAnswer but the row's tool_name is not "ask_coordinator".
	ErrAnswerOnNonAsk = errors.New("approvals: decision=answer is for ask-variant approvals only")

	// ErrInvalidScope is returned by PolicyStore.Set when the supplied
	// scope does not match the allowed grammar (global | skill:<name>
	// | role:<skill>:<role>).
	ErrInvalidScope = errors.New("approvals: invalid policy scope")

	// ErrInvalidPolicy is returned by PolicyStore.Set when the policy
	// is outside the recognised enum.
	ErrInvalidPolicy = errors.New("approvals: invalid policy")

	// ErrInvalidToolName is returned by PolicyStore.Set when the
	// tool_name is empty or consists of only the wildcard "*".
	ErrInvalidToolName = errors.New("approvals: invalid tool name")

	// ErrUnknownSession is returned by Manager.Request when one of
	// the supplied session ids does not resolve.
	ErrUnknownSession = errors.New("approvals: unknown session")

	// ErrInvalidRisk is returned by Manager.Request when the risk
	// classification is outside the recognised enum.
	ErrInvalidRisk = errors.New("approvals: invalid risk")

	// ErrForbiddenForSubAgent is returned by coordinator-only tools
	// (approval_respond, policy_*) when invoked from a sub-agent.
	ErrForbiddenForSubAgent = errors.New("approvals: tool is coordinator-only")

	// ErrForbiddenForCoordinator is returned by sub-agent-only tools
	// (ask_coordinator) when invoked from a coordinator session.
	ErrForbiddenForCoordinator = errors.New("approvals: tool is sub-agent-only")
)
