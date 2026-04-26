package approvals

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/adk/tool"

	apstore "github.com/hugr-lab/hugen/pkg/approvals/store"
	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
)

// Config is the package-internal subset of pkg/Config
// the manager consumes. Backend-level config (DestructiveTools etc.)
// is read directly from this struct. The runtime translates from
// Config at startup so pkg/approvals stays free of
// any pkg/config dependency.
type Config struct {
	// DefaultTimeout — pending approvals older than this become
	// `expired` on the next sweeper tick. Zero falls back to 30m.
	DefaultTimeout time.Duration

	// SafePolicyChange controls the widening detector at Gate Step 2.
	// nil ⇒ default true.
	SafePolicyChange *bool

	// EnableImpactEstimators turns on per-tool impact estimators
	// for the envelope (currently unused — phase-4 ships none).
	EnableImpactEstimators bool

	// DestructiveTools is the operator-managed list that bumps the
	// hardcoded default to `manual_required` for matching tool names.
	DestructiveTools []string
}

// ServiceName is the provider name *Manager registers under in
// tools.Manager. Coordinator-only by Bind authorization.
const ServiceName = "_approvals"

// sessionEventWriter is the minimal session-event surface the Manager
// uses to emit approval_requested / approval_responded /
// policy_changed / ask_coordinator events. Satisfied by
// *sessstore.Client in production.
type sessionEventWriter interface {
	AppendEvent(ctx context.Context, ev sessstore.Event) (string, error)
}

// MissionStatusUpdater flips the mission row's status (e.g. to
// "waiting" when the Gate decides Manual). The Manager does not own
// the missions store; this slim interface lets it call out to
// *missions/store.Store without import cycles. Method name matches
// missionsstore.Store.MarkStatus so the store satisfies the
// interface directly. nil-safe — when not wired, the manager logs
// a warning and skips the flip.
type MissionStatusUpdater interface {
	MarkStatus(ctx context.Context, missionID, status string) error
}

// Deps bundles the manager's external dependencies. Constructed once
// at runtime startup; passed through New.
type Deps struct {
	// Store is the typed approvals + tool_policies store. Required.
	Store *apstore.Client

	// SessionEvents writes lifecycle events on the coordinator and
	// mission sessions. Required.
	SessionEvents sessionEventWriter

	// Missions transitions the gated mission to `waiting`. Optional —
	// when nil, the Manager logs a warning and skips the status flip.
	// Production runtime should always wire this.
	Missions MissionStatusUpdater

	// AgentID scopes everything to a single agent. Required.
	AgentID string

	// Logger defaults to slog.Default() when nil.
	Logger *slog.Logger

	// Now overrides time.Now for tests; defaults to time.Now.UTC.
	Now func() time.Time
}

// Manager is the public surface of the HITL approvals subsystem.
// Implements tools.Provider directly (Name / Tools); domain methods
// (Request / Respond / Get / List / SweepExpired / Gate) are
// receiver methods on the same struct. Mirrors pkg/artifacts.Manager
// (phase 3) and pkg/memory/service.go.
type Manager struct {
	cfg      Config
	store    *apstore.Client
	events   sessionEventWriter
	missions MissionStatusUpdater
	agentID  string
	logger   *slog.Logger
	nowFn    func() time.Time

	// PolicyStore caches tool_policies in memory; consulted by the
	// Gate on every sub-agent tool call. Constructor blocks on the
	// initial Refresh.
	policy *PolicyStore
}

// New constructs the Manager. Returns an error when required deps
// are missing or when the initial PolicyStore refresh fails.
func New(cfg Config, deps Deps) (*Manager, error) {
	if deps.Store == nil {
		return nil, fmt.Errorf("approvals: New requires Store")
	}
	if deps.SessionEvents == nil {
		return nil, fmt.Errorf("approvals: New requires SessionEvents")
	}
	if deps.AgentID == "" {
		return nil, fmt.Errorf("approvals: New requires AgentID")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}

	policy, err := newPolicyStore(deps.Store, deps.AgentID, deps.Logger)
	if err != nil {
		return nil, fmt.Errorf("approvals: init policy store: %w", err)
	}
	if err := policy.Refresh(context.Background()); err != nil {
		return nil, fmt.Errorf("approvals: refresh policy store: %w", err)
	}

	return &Manager{
		cfg:      cfg,
		store:    deps.Store,
		events:   deps.SessionEvents,
		missions: deps.Missions,
		agentID:  deps.AgentID,
		logger:   deps.Logger,
		nowFn:    deps.Now,
		policy:   policy,
	}, nil
}

// AgentID returns the agent scope the Manager was constructed for.
func (m *Manager) AgentID() string { return m.agentID }

// Config returns a copy of the operator-tuned ApprovalsConfig.
func (m *Manager) Config() Config { return m.cfg }

// PolicyStore exposes the hot-cached tool_policies view consulted by
// the Gate. Tools (`policy_list`, `policy_set`, `policy_remove`)
// also call into it. Never nil after a successful New.
func (m *Manager) PolicyStore() *PolicyStore { return m.policy }

// ─────────────────────────────────────────────────────────────────
// Domain methods
// ─────────────────────────────────────────────────────────────────

// Request inserts a new pending approvals row, emits the
// approval_requested event on coord + mission sessions (and
// ask_coordinator on the mission session for ask-variant calls),
// and transitions the mission to `waiting` (when missions is wired).
//
// All four side effects fire in close succession; production wraps
// them in a single GraphQL mutation document at a higher layer.
func (m *Manager) Request(ctx context.Context, payload RequestPayload) (ApprovalRef, error) {
	if payload.MissionSessionID == "" {
		return ApprovalRef{}, ErrUnknownSession
	}
	if payload.CoordSessionID == "" {
		return ApprovalRef{}, ErrUnknownSession
	}
	if payload.ToolName == "" {
		return ApprovalRef{}, fmt.Errorf("approvals: tool_name required")
	}
	if err := payload.Risk.Validate(); err != nil {
		return ApprovalRef{}, fmt.Errorf("%w: %v", ErrInvalidRisk, err)
	}
	agentID := payload.AgentID
	if agentID == "" {
		agentID = m.agentID
	}

	id := NewApprovalID()
	now := m.nowFn()

	rec := apstore.ApprovalRecord{
		ID:               id,
		AgentID:          agentID,
		MissionSessionID: payload.MissionSessionID,
		CoordSessionID:   payload.CoordSessionID,
		ToolName:         payload.ToolName,
		Args:             payload.Args,
		Risk:             string(payload.Risk),
		Status:           string(StatusPending),
	}
	if err := m.store.InsertApproval(ctx, rec); err != nil {
		return ApprovalRef{}, fmt.Errorf("approvals: insert: %w", err)
	}

	// Build envelope metadata.
	hitlKind := HITLKindApproval
	choices := []string{"approve", "reject", "modify"}
	if payload.Source == RequestFromAsk {
		hitlKind = HITLKindAsk
		choices = []string{"answer"}
	}
	envMeta := EnvelopeMetadata{
		HITLKind:   hitlKind,
		ApprovalID: id,
		MissionID:  payload.MissionSessionID,
		ToolName:   payload.ToolName,
		Risk:       payload.Risk,
		Choices:    choices,
		ArgsDigest: argsDigest(payload.Args),
	}
	if payload.Source == RequestFromAsk {
		if q, ok := payload.Args["question"].(string); ok {
			// Ask envelope swaps args_digest for the raw question.
			envMeta.ArgsDigest = truncateRunes(q, 200)
		}
		if sug, ok := payload.Args["suggested"].([]string); ok {
			envMeta.Suggested = sug
		} else if sugAny, ok := payload.Args["suggested"].([]any); ok {
			for _, v := range sugAny {
				if s, ok := v.(string); ok {
					envMeta.Suggested = append(envMeta.Suggested, s)
				}
			}
		}
	}
	body := renderEnvelopeBody(envMeta, payload.Args)

	// Emit on coordinator session.
	coordEvent := sessstore.Event{
		SessionID: payload.CoordSessionID,
		AgentID:   agentID,
		EventType: sessstore.EventTypeApprovalRequested,
		Author:    "system",
		ToolName:  payload.ToolName,
		Content:   body,
		Metadata:  envMeta.ToMap(),
	}
	if _, err := m.events.AppendEvent(ctx, coordEvent); err != nil {
		m.logger.WarnContext(ctx, "approvals: emit approval_requested on coord", "id", id, "err", err)
	}

	// Emit on mission session for completeness.
	missionEvent := coordEvent
	missionEvent.SessionID = payload.MissionSessionID
	if _, err := m.events.AppendEvent(ctx, missionEvent); err != nil {
		m.logger.WarnContext(ctx, "approvals: emit approval_requested on mission", "id", id, "err", err)
	}

	// Ask-variant emits an additional ask_coordinator event on the
	// sub-agent's own session so Reviewer / debug tools can
	// distinguish "voluntarily escalated" from "gated by Gate".
	if payload.Source == RequestFromAsk {
		askMeta := AskCoordinatorMeta{
			ApprovalID: id,
			Question:   stringFromArgs(payload.Args, "question"),
			Suggested:  envMeta.Suggested,
		}
		askEvent := sessstore.Event{
			SessionID: payload.MissionSessionID,
			AgentID:   agentID,
			EventType: sessstore.EventTypeAskCoordinator,
			Author:    "subagent",
			ToolName:  "ask_coordinator",
			Content:   "Asking: " + truncateRunes(askMeta.Question, 160),
			Metadata: map[string]any{
				"approval_id": askMeta.ApprovalID,
				"question":    askMeta.Question,
				"suggested":   askMeta.Suggested,
			},
		}
		if _, err := m.events.AppendEvent(ctx, askEvent); err != nil {
			m.logger.WarnContext(ctx, "approvals: emit ask_coordinator", "id", id, "err", err)
		}
	}

	// Transition mission to waiting.
	if m.missions != nil {
		if err := m.missions.MarkStatus(ctx, payload.MissionSessionID, "waiting"); err != nil {
			m.logger.WarnContext(ctx, "approvals: flip mission to waiting", "id", id, "err", err)
		}
	}

	return ApprovalRef{
		ID:        id,
		Status:    StatusPending,
		CreatedAt: now,
	}, nil
}

// Respond resolves a pending approvals row. Validates decision +
// payload constraints, updates the row, emits approval_responded on
// the coord session.
//
// On success the mission's resume happens later on the next scheduler
// tick — Respond does not directly transition the mission row.
func (m *Manager) Respond(ctx context.Context, payload RespondPayload) (ApprovalRef, error) {
	if payload.ApprovalID == "" {
		return ApprovalRef{}, fmt.Errorf("approvals: ApprovalID required")
	}
	if err := payload.Decision.Validate(); err != nil {
		return ApprovalRef{}, fmt.Errorf("%w: %v", ErrInvalidDecision, err)
	}
	if payload.Decision == DecisionModify && len(payload.ModifiedArgs) == 0 {
		return ApprovalRef{}, ErrModifiedArgsMissing
	}
	if payload.Decision == DecisionAnswer && payload.Answer == "" {
		return ApprovalRef{}, ErrAnswerMissing
	}

	existing, err := m.store.GetApproval(ctx, payload.ApprovalID)
	if err != nil {
		if errors.Is(err, apstore.ErrApprovalNotFound) {
			return ApprovalRef{}, ErrApprovalNotFound
		}
		return ApprovalRef{}, fmt.Errorf("approvals: get: %w", err)
	}
	if Status(existing.Status).IsTerminal() {
		if existing.Status == string(StatusExpired) {
			return ApprovalRef{}, ErrApprovalExpired
		}
		return ApprovalRef{}, ErrAlreadyResolved
	}
	if payload.Decision == DecisionAnswer && existing.ToolName != "ask_coordinator" {
		return ApprovalRef{}, ErrAnswerOnNonAsk
	}

	// Map decision → terminal status.
	target := StatusApproved
	switch payload.Decision {
	case DecisionReject:
		target = StatusRejected
	case DecisionModify:
		target = StatusModified
	case DecisionApprove, DecisionAnswer:
		target = StatusApproved
	}

	// Build response payload. Note: ask-variants store the answer in
	// response.answer; tool-call gates store decision/note/modified_args.
	resp := Response{
		Decision:    payload.Decision,
		Note:        payload.Note,
		ResponderID: payload.ResponderID,
	}
	if payload.Decision == DecisionModify {
		resp.ModifiedArgs = payload.ModifiedArgs
	}
	if payload.Decision == DecisionAnswer {
		resp.Answer = payload.Answer
	}
	respMap := responseToMap(resp)

	now := m.nowFn()
	if err := m.store.UpdateStatus(ctx, payload.ApprovalID, string(target), respMap, now); err != nil {
		switch {
		case errors.Is(err, apstore.ErrApprovalNotFound):
			return ApprovalRef{}, ErrApprovalNotFound
		case errors.Is(err, apstore.ErrApprovalExpired):
			return ApprovalRef{}, ErrApprovalExpired
		case errors.Is(err, apstore.ErrAlreadyResolved):
			return ApprovalRef{}, ErrAlreadyResolved
		default:
			return ApprovalRef{}, fmt.Errorf("approvals: update: %w", err)
		}
	}

	// On rejection, transition the gated mission to `cancelled` so
	// observers see a clear terminal state. Approve / modify keep the
	// mission in `waiting` — coord LLM is responsible for re-dispatch
	// per constitution rules.
	if target == StatusRejected && m.missions != nil {
		if err := m.missions.MarkStatus(ctx, existing.MissionSessionID, "cancelled"); err != nil {
			m.logger.WarnContext(ctx, "approvals: cancel mission on reject",
				"approval", payload.ApprovalID, "mission", existing.MissionSessionID, "err", err)
		}
	}

	// Emit approval_responded on coord session.
	respMeta := ApprovalRespondedMeta{
		ApprovalID: payload.ApprovalID,
		Decision:   string(target),
		Note:       payload.Note,
	}
	if payload.Decision == DecisionModify {
		respMeta.ModifiedArgs = payload.ModifiedArgs
	}
	if payload.Decision == DecisionAnswer {
		respMeta.Answer = payload.Answer
	}
	author := payload.ResponderID
	if author == "" {
		author = "user"
	}
	if _, err := m.events.AppendEvent(ctx, sessstore.Event{
		SessionID: existing.CoordSessionID,
		AgentID:   existing.AgentID,
		EventType: sessstore.EventTypeApprovalResponded,
		Author:    author,
		Content:   fmt.Sprintf("Approval %s %s", payload.ApprovalID, target),
		Metadata: map[string]any{
			"approval_id":   payload.ApprovalID,
			"decision":      string(target),
			"modified_args": payload.ModifiedArgs,
			"answer":        payload.Answer,
			"note":          payload.Note,
		},
	}); err != nil {
		m.logger.WarnContext(ctx, "approvals: emit approval_responded", "id", payload.ApprovalID, "err", err)
	}

	return ApprovalRef{
		ID:        payload.ApprovalID,
		Status:    target,
		CreatedAt: existing.CreatedAt,
	}, nil
}

// Get returns the full Approval row by id.
func (m *Manager) Get(ctx context.Context, id string) (Approval, error) {
	rec, err := m.store.GetApproval(ctx, id)
	if err != nil {
		if errors.Is(err, apstore.ErrApprovalNotFound) {
			return Approval{}, ErrApprovalNotFound
		}
		return Approval{}, fmt.Errorf("approvals: get: %w", err)
	}
	return recordToApproval(rec), nil
}

// ListPending returns the pending approvals visible to coordSessionID,
// ordered by created_at DESC. Convenience over List that hardcodes
// status=pending and the coord-session filter — used by the
// pending_approvals tool which is the coord-side discovery surface.
//
// limit defaults to 20 when 0; max 200.
func (m *Manager) ListPending(ctx context.Context, coordSessionID string, limit int) ([]Approval, error) {
	if coordSessionID == "" {
		return nil, fmt.Errorf("approvals: ListPending requires coordSessionID")
	}
	return m.List(ctx, ListFilter{
		CoordSessionID: coordSessionID,
		Statuses:       []Status{StatusPending},
		Limit:          limit,
	})
}

// List returns approvals matching the filter, ordered by created_at DESC.
func (m *Manager) List(ctx context.Context, f ListFilter) ([]Approval, error) {
	statuses := make([]string, 0, len(f.Statuses))
	for _, s := range f.Statuses {
		statuses = append(statuses, string(s))
	}
	if len(statuses) == 0 {
		statuses = []string{string(StatusPending)}
	}
	rows, err := m.store.ListApprovals(ctx, apstore.ListFilter{
		CoordSessionID: f.CoordSessionID,
		Statuses:       statuses,
		Limit:          f.Limit,
	})
	if err != nil {
		return nil, fmt.Errorf("approvals: list: %w", err)
	}
	out := make([]Approval, 0, len(rows))
	for _, r := range rows {
		out = append(out, recordToApproval(r))
	}
	return out, nil
}

// Gate returns a *Gate view on this manager. Methods on Gate are
// receiver functions on the manager; the *Gate type is a thin
// wrapper for type-disambiguation at call sites.
func (m *Manager) Gate() *Gate { return &Gate{m: m} }

// ─────────────────────────────────────────────────────────────────
// Internal helpers
// ─────────────────────────────────────────────────────────────────

func recordToApproval(rec apstore.ApprovalRecord) Approval {
	a := Approval{
		ID:               rec.ID,
		AgentID:          rec.AgentID,
		MissionSessionID: rec.MissionSessionID,
		CoordSessionID:   rec.CoordSessionID,
		ToolName:         rec.ToolName,
		Args:             rec.Args,
		Risk:             Risk(rec.Risk),
		Status:           Status(rec.Status),
		CreatedAt:        rec.CreatedAt,
		RespondedAt:      rec.RespondedAt,
	}
	if rec.Response != nil {
		r := Response{}
		if v, ok := rec.Response["decision"].(string); ok {
			r.Decision = Decision(v)
		}
		if v, ok := rec.Response["modified_args"].(map[string]any); ok {
			r.ModifiedArgs = v
		}
		if v, ok := rec.Response["note"].(string); ok {
			r.Note = v
		}
		if v, ok := rec.Response["answer"].(string); ok {
			r.Answer = v
		}
		if v, ok := rec.Response["responder_id"].(string); ok {
			r.ResponderID = v
		}
		a.Response = &r
	}
	return a
}

func responseToMap(r Response) map[string]any {
	m := map[string]any{
		"decision": string(r.Decision),
	}
	if r.Note != "" {
		m["note"] = r.Note
	}
	if r.Answer != "" {
		m["answer"] = r.Answer
	}
	if r.ResponderID != "" {
		m["responder_id"] = r.ResponderID
	}
	if len(r.ModifiedArgs) > 0 {
		m["modified_args"] = r.ModifiedArgs
	}
	return m
}

// stringFromArgs is a small helper for extracting a string field
// from a JSON-decoded map[string]any. Returns "" when missing or
// not-a-string.
func stringFromArgs(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// ─────────────────────────────────────────────────────────────────
// tools.Provider implementation
// ─────────────────────────────────────────────────────────────────

// Name returns the provider name registered with tools.Manager.
func (m *Manager) Name() string { return ServiceName }

// Tools returns the five HITL/policy tools owned by this manager.
// Coordinator-vs-sub-agent gating happens inside each tool's Run via
// session_type lookup.
//
// Phase 4 ships:
//   - approval_respond (coordinator-only) — US1
//   - ask_coordinator  (sub-agent-only)  — US3
//   - policy_list / policy_set / policy_remove (coordinator-only) — US2
//
// US1 ships approval_respond + pending_approvals;
// US2 adds policy_list / policy_set / policy_remove;
// US3 will add ask_coordinator.
func (m *Manager) Tools() []tool.Tool {
	return []tool.Tool{
		&approvalRespondTool{m: m},
		&pendingApprovalsTool{m: m},
		&policyListTool{m: m},
		&policySetTool{m: m},
		&policyRemoveTool{m: m},
	}
}

// IsDestructiveTool reports whether the named tool sits in the
// operator-tuned destructive-tools list. Used by the Gate as the
// final fallback when neither policy nor frontmatter matched —
// destructive tools default to manual_required, others to
// always_allowed.
func (m *Manager) IsDestructiveTool(toolName string) bool {
	for _, t := range m.cfg.DestructiveTools {
		if t == toolName {
			return true
		}
	}
	return false
}

// EmitPolicyChanged writes a policy_changed event on the given
// coord session. Called by the policy_* tool bodies after a
// successful PolicyStore mutation. Lives on Manager (not on
// PolicyStore) because the events package is a Manager dep, not a
// store one.
func (m *Manager) EmitPolicyChanged(ctx context.Context, coordSessionID string, meta PolicyChangedMeta) {
	if coordSessionID == "" {
		coordSessionID = m.agentID // best-effort fallback for un-scoped writers
	}
	if _, err := m.events.AppendEvent(ctx, sessstore.Event{
		SessionID: coordSessionID,
		AgentID:   m.agentID,
		EventType: sessstore.EventTypePolicyChanged,
		Author:    meta.CreatedBy,
		Content:   fmt.Sprintf("Policy %s @ %s: %s → %s", meta.ToolName, meta.Scope, meta.OldPolicy, meta.NewPolicy),
		Metadata: map[string]any{
			"tool_name":   meta.ToolName,
			"scope":       meta.Scope,
			"old_policy":  meta.OldPolicy,
			"new_policy":  meta.NewPolicy,
			"note":        meta.Note,
			"created_by":  meta.CreatedBy,
		},
	}); err != nil {
		m.logger.WarnContext(ctx, "approvals: emit policy_changed",
			"tool", meta.ToolName, "scope", meta.Scope, "err", err)
	}
}

// ToolCall is the input to Gate.Check + PolicyStore.Resolve. Lives
// in manager.go to keep the central ToolCall shape adjacent to the
// Manager type that consumes it; the *PolicyStore implementation
// itself lives in policy.go.
type ToolCall struct {
	AgentID        string
	SessionID      string
	CoordSessionID string
	ToolName       string
	Args           map[string]any
	Skill          string
	Role           string
	Frontmatter    *FrontmatterApprovalRules

	// InternalBypass is executor-set only; never from LLM. Used on
	// resume from a meta-approval (US2 / research §7).
	InternalBypass GateBypass
}

// GateBypass is the executor-internal flag bag for resume paths.
type GateBypass struct {
	SafePolicyChange bool
}
