package approvals

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// IDPrefix is the prefix all approval ids carry. Mirrors the "art-"
// pattern shipped by phase 3 for artifacts.
const IDPrefix = "app-"

// NewApprovalID returns a fresh approval id of the form
// "app-" + 12 hex characters drawn from crypto/rand. Collision
// probability is negligible at registry scale (10K rows ≈ 1 in 5×10⁹).
func NewApprovalID() string {
	var buf [6]byte
	_, _ = rand.Read(buf[:]) // crypto/rand never returns an error
	return IDPrefix + hex.EncodeToString(buf[:])
}

// Risk classifies the user-visible severity of a gated tool call.
// Set by the role's approval_rules declaration or — when no rule
// matches — derived from cfg.Approvals.DestructiveTools membership.
type Risk string

const (
	RiskLow    Risk = "low"
	RiskMedium Risk = "medium"
	RiskHigh   Risk = "high"
)

// Validate reports whether r is one of the recognised values.
func (r Risk) Validate() error {
	switch r {
	case RiskLow, RiskMedium, RiskHigh:
		return nil
	default:
		return fmt.Errorf("approvals: invalid risk %q (want low|medium|high)", r)
	}
}

// Status is the lifecycle state of an approvals row. Pending is the
// only non-terminal state; the four terminal statuses are absorbing.
type Status string

const (
	StatusPending  Status = "pending"
	StatusApproved Status = "approved"
	StatusRejected Status = "rejected"
	StatusModified Status = "modified"
	StatusExpired  Status = "expired"
)

// Validate reports whether s is one of the recognised values.
func (s Status) Validate() error {
	switch s {
	case StatusPending, StatusApproved, StatusRejected, StatusModified, StatusExpired:
		return nil
	default:
		return fmt.Errorf("approvals: invalid status %q", s)
	}
}

// IsTerminal reports whether s is an absorbing state.
func (s Status) IsTerminal() bool {
	return s == StatusApproved || s == StatusRejected || s == StatusModified || s == StatusExpired
}

// Decision is the user-supplied resolution carried in an
// approval_respond call. Maps onto Status (modulo "answered" for
// ask-variants which lands as StatusApproved with response.answer
// populated).
type Decision string

const (
	DecisionApprove Decision = "approve"
	DecisionReject  Decision = "reject"
	DecisionModify  Decision = "modify"
	DecisionAnswer  Decision = "answer"
)

// Validate reports whether d is one of the recognised values.
func (d Decision) Validate() error {
	switch d {
	case DecisionApprove, DecisionReject, DecisionModify, DecisionAnswer:
		return nil
	default:
		return fmt.Errorf("approvals: invalid decision %q (want approve|reject|modify|answer)", d)
	}
}

// HITLKind discriminates between tool-call gates and free-form
// ask-coordinator questions in the envelope metadata.
type HITLKind string

const (
	HITLKindApproval HITLKind = "approval"
	HITLKindAsk      HITLKind = "ask"
)

// RequestSource discriminates between the two callers of
// Manager.Request: the Gate (tool-call gate) and the
// ask_coordinator tool (sub-agent question).
type RequestSource int

const (
	// RequestFromGate flags a tool-call gate insertion.
	RequestFromGate RequestSource = 0
	// RequestFromAsk flags an ask_coordinator insertion. The Manager
	// emits an additional ask_coordinator event on the sub-agent's
	// own session in addition to the standard approval_requested
	// events.
	RequestFromAsk RequestSource = 1
)

// RequestPayload is the input to Manager.Request.
type RequestPayload struct {
	AgentID          string
	MissionSessionID string
	CoordSessionID   string
	ToolName         string
	Args             map[string]any
	Risk             Risk
	Source           RequestSource

	// Skill / Role / Frontmatter let the Gate annotate the request
	// for resolution-chain debugging. Optional; not persisted.
	Skill       string
	Role        string
	Frontmatter *FrontmatterApprovalRules
}

// RespondPayload is the input to Manager.Respond.
type RespondPayload struct {
	ApprovalID   string
	Decision     Decision
	ModifiedArgs map[string]any
	Note         string
	Answer       string

	// ResponderID identifies the actor: "user" for user-driven
	// (relayed via approval_respond), "coord_llm" for LLM-driven
	// answers, "system" for sweeper-driven expirations.
	ResponderID string

	// BypassPolicySafety is an internal-only flag set by the
	// executor on resume from a meta-approval (research §7). It
	// instructs the Gate to skip safe_policy_change widening
	// detection for the resumed call. Never exposed via tool args.
	BypassPolicySafety bool
}

// Approval is the full row representation, returned by Manager.Get
// and consumed by the executor's resume path.
type Approval struct {
	ID               string
	AgentID          string
	MissionSessionID string
	CoordSessionID   string
	ToolName         string
	Args             map[string]any
	Risk             Risk
	Status           Status
	Response         *Response
	CreatedAt        time.Time
	RespondedAt      *time.Time
}

// ApprovalRef is the slim reference returned by Request / Respond.
type ApprovalRef struct {
	ID        string
	Status    Status
	CreatedAt time.Time
}

// Response is the structured contents of approvals.response.
type Response struct {
	Decision     Decision       `json:"decision"`
	ModifiedArgs map[string]any `json:"modified_args,omitempty"`
	Note         string         `json:"note,omitempty"`
	Answer       string         `json:"answer,omitempty"`
	ResponderID  string         `json:"responder_id,omitempty"`
}

// ListFilter narrows Manager.List results.
type ListFilter struct {
	CoordSessionID string   // optional; filter by surfacing coordinator
	Statuses       []Status // optional; default: [Pending]
	Limit          int      // default 50; max 200
}

// FrontmatterApprovalRules mirrors the role-frontmatter shape parsed
// by skills/<skill>/SKILL.md. Optional; pointer can be nil when the
// caller has no frontmatter context.
type FrontmatterApprovalRules struct {
	AutoApprove       []string `yaml:"auto_approve,omitempty"`
	RequireUser       []string `yaml:"require_user,omitempty"`
	ParentCanApprove  []string `yaml:"parent_can_approve,omitempty"`
	RiskOverrides     map[string]Risk `yaml:"risk,omitempty"`
}

// EnvelopeMetadata is the structured-metadata blob attached to
// approval_requested events on the coord and mission sessions.
// Mirrors contracts/envelope.md.
type EnvelopeMetadata struct {
	HITLKind        HITLKind       `json:"hitl_kind"`
	ApprovalID      string         `json:"approval_id"`
	MissionID       string         `json:"mission_id"`
	ToolName        string         `json:"tool_name"`
	Risk            Risk           `json:"risk"`
	Choices         []string       `json:"choices"`
	ArgsDigest      string         `json:"args_digest,omitempty"`
	EstimatedImpact map[string]any `json:"estimated_impact,omitempty"`
	Suggested       []string       `json:"suggested,omitempty"`
}

// ApprovalRespondedMeta is the metadata payload for an
// approval_responded event.
type ApprovalRespondedMeta struct {
	ApprovalID   string         `json:"approval_id"`
	Decision     string         `json:"decision"` // approved | rejected | modified | expired | answered
	ModifiedArgs map[string]any `json:"modified_args,omitempty"`
	Answer       string         `json:"answer,omitempty"`
	Note         string         `json:"note,omitempty"`
}

// PolicyChangedMeta is the metadata payload for a policy_changed
// event.
type PolicyChangedMeta struct {
	ToolName  string `json:"tool_name"`
	Scope     string `json:"scope"`
	OldPolicy string `json:"old_policy"` // "<none>" when the row did not exist before
	NewPolicy string `json:"new_policy"` // "removed" when policy_remove fired
	Note      string `json:"note,omitempty"`
	CreatedBy string `json:"created_by"`
}

// AskCoordinatorMeta is the metadata payload for an ask_coordinator
// event emitted on the sub-agent's own session.
type AskCoordinatorMeta struct {
	ApprovalID string   `json:"approval_id"`
	Question   string   `json:"question"`
	Suggested  []string `json:"suggested,omitempty"`
}
