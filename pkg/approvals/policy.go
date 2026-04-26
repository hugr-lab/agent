package approvals

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	apstore "github.com/hugr-lab/hugen/pkg/approvals/store"
)

// PolicyDecision mirrors the persisted `policy` column enum.
type PolicyDecision int

const (
	// PolicyAlwaysAllowed — tool dispatches without gate intervention.
	PolicyAlwaysAllowed PolicyDecision = iota
	// PolicyManualRequired — gate hits Manual; user approval required.
	PolicyManualRequired
	// PolicyDenied — gate hits Deny; tool refused outright.
	PolicyDenied
)

// String returns the canonical wire form ("always_allowed" /
// "manual_required" / "denied").
func (d PolicyDecision) String() string {
	switch d {
	case PolicyAlwaysAllowed:
		return "always_allowed"
	case PolicyManualRequired:
		return "manual_required"
	case PolicyDenied:
		return "denied"
	default:
		return "unknown"
	}
}

// ParsePolicyDecision converts the wire form back into the enum.
// Returns an error for unrecognised values.
func ParsePolicyDecision(s string) (PolicyDecision, error) {
	switch s {
	case "always_allowed":
		return PolicyAlwaysAllowed, nil
	case "manual_required":
		return PolicyManualRequired, nil
	case "denied":
		return PolicyDenied, nil
	default:
		return 0, fmt.Errorf("%w: %q", ErrInvalidPolicy, s)
	}
}

// PolicyOrigin records where the resolution chain matched. Used for
// audit / debug logging — every Decision the Gate returns can name
// the row that produced it.
type PolicyOrigin int

const (
	// OriginCache — matched a tool_policies row.
	OriginCache PolicyOrigin = iota
	// OriginFrontmatter — matched the role's `approval_rules` block.
	OriginFrontmatter
	// OriginDefault — hardcoded fallback (DestructiveTools or all-allow).
	OriginDefault
)

// ResolvedPolicy is the output of PolicyStore.Resolve.
type ResolvedPolicy struct {
	Policy PolicyDecision
	Origin string // human-readable: "role:hugr-data:data_analyst exact" | "skill:foo prefix" | ...
	Source PolicyOrigin
	// Risk is populated when the matched source carries one (currently
	// only frontmatter risk_overrides). Default RiskMedium for manual
	// matches; empty for allowed/denied.
	Risk Risk
}

// Policy is the public Go-side mirror of a tool_policies row used
// by Set / Snapshot. Equivalent to apstore.PolicyRecord but with
// the typed PolicyDecision instead of a raw string.
type Policy struct {
	AgentID   string
	ToolName  string
	Scope     string // "global" | "skill:<name>" | "role:<skill>:<role>"
	Decision  PolicyDecision
	Note      string
	CreatedBy string // "user" | "llm" | "system"
}

// PolicyStore caches tool_policies in memory. Reads are lock-free
// via atomic.Pointer[snapshot]; writes serialize on a writer mutex.
//
// The resolution chain (Resolve) walks the snapshot in this order
// and returns the first match:
//
//   1. role:<skill>:<role>  exact tool_name
//   2. role:<skill>:<role>  prefix glob (data-*)
//   3. skill:<skill>        exact
//   4. skill:<skill>        prefix
//   5. global               exact
//   6. global               prefix
//   7. frontmatter approval_rules (auto_approve / require_user / parent_can_approve)
//   8. hardcoded default (auto_approve unless tool ∈ DestructiveTools)
//
// Step 7 reads from ToolCall.Frontmatter; step 8 reads from the
// Manager's destructiveTools set (passed in at construction).
type PolicyStore struct {
	store    *apstore.Client
	agentID  string
	logger   *slog.Logger
	mu       sync.Mutex
	snapshot atomic.Pointer[policySnapshot]
}

// policySnapshot is the immutable read-only view consulted by
// Resolve. Built from the DB row set; replaced atomically on every
// successful Set / Remove / Refresh.
type policySnapshot struct {
	// exact maps (scope, tool_name) → Decision for instant lookup of
	// chain steps 1, 3, 5.
	exact map[snapshotKey]ResolvedRow

	// prefix is sorted by (scope priority, prefix length DESC) so the
	// longest matching glob wins per scope. Walked for chain steps 2,
	// 4, 6.
	prefix []prefixEntry
}

type snapshotKey struct {
	Scope    string
	ToolName string
}

// ResolvedRow is the cached representation of one tool_policies row
// after parsing the policy column into the typed enum.
type ResolvedRow struct {
	Scope     string
	ToolName  string
	Decision  PolicyDecision
	Note      string
	CreatedBy string
	UpdatedAt time.Time
}

type prefixEntry struct {
	Scope    string
	Prefix   string // "data-" (the trailing * is stripped at build time)
	Decision PolicyDecision
	Origin   string
}

// scopePriority returns the chain-traversal priority for a scope.
// Lower is higher priority. Used to sort prefixEntry so role-scoped
// prefixes are checked before skill-scoped, etc.
func scopePriority(scope string) int {
	switch {
	case strings.HasPrefix(scope, "role:"):
		return 0
	case strings.HasPrefix(scope, "skill:"):
		return 1
	case scope == "global":
		return 2
	default:
		return 3 // unknown scope — sort last
	}
}

// newPolicyStore constructs the cache. Refresh must be called
// before the cache is consulted (Manager.New does this in its
// constructor flow).
func newPolicyStore(client *apstore.Client, agentID string, logger *slog.Logger) (*PolicyStore, error) {
	if client == nil {
		return nil, fmt.Errorf("approvals/policy: nil store client")
	}
	ps := &PolicyStore{
		store:   client,
		agentID: agentID,
		logger:  logger,
	}
	ps.snapshot.Store(&policySnapshot{
		exact:  map[snapshotKey]ResolvedRow{},
		prefix: nil,
	})
	return ps, nil
}

// Refresh reloads the snapshot from the DB. Called once at
// construction; not normally called again under steady state
// (Set / Remove rebuild the snapshot inline).
func (p *PolicyStore) Refresh(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	rows, err := p.store.LoadAllPolicies(ctx)
	if err != nil {
		return fmt.Errorf("approvals/policy: load all: %w", err)
	}
	snap, err := buildSnapshot(rows)
	if err != nil {
		return err
	}
	p.snapshot.Store(snap)
	return nil
}

// Snapshot returns the current snapshot pointer. Safe for concurrent
// reads. The returned pointer MUST NOT be mutated.
func (p *PolicyStore) Snapshot() *policySnapshot {
	return p.snapshot.Load()
}

// Resolve walks the resolution chain and returns the first match.
// Pure read; no DB call.
func (p *PolicyStore) Resolve(_ context.Context, call ToolCall) ResolvedPolicy {
	snap := p.snapshot.Load()

	// Step 1-2: role:<skill>:<role> scope.
	if call.Skill != "" && call.Role != "" {
		scope := "role:" + call.Skill + ":" + call.Role
		if r, ok := snap.exact[snapshotKey{Scope: scope, ToolName: call.ToolName}]; ok {
			return resolvedFromRow(r, fmt.Sprintf("%s exact", scope))
		}
		if entry, ok := snap.prefixMatch(scope, call.ToolName); ok {
			return resolvedFromEntry(entry)
		}
	}

	// Step 3-4: skill:<skill> scope.
	if call.Skill != "" {
		scope := "skill:" + call.Skill
		if r, ok := snap.exact[snapshotKey{Scope: scope, ToolName: call.ToolName}]; ok {
			return resolvedFromRow(r, fmt.Sprintf("%s exact", scope))
		}
		if entry, ok := snap.prefixMatch(scope, call.ToolName); ok {
			return resolvedFromEntry(entry)
		}
	}

	// Step 5-6: global scope.
	if r, ok := snap.exact[snapshotKey{Scope: "global", ToolName: call.ToolName}]; ok {
		return resolvedFromRow(r, "global exact")
	}
	if entry, ok := snap.prefixMatch("global", call.ToolName); ok {
		return resolvedFromEntry(entry)
	}

	// Step 7: frontmatter approval_rules.
	if call.Frontmatter != nil {
		fm := call.Frontmatter
		if matchAny(call.ToolName, fm.RequireUser) || matchAny(call.ToolName, fm.ParentCanApprove) {
			risk := RiskMedium
			if r, ok := fm.RiskOverrides[call.ToolName]; ok && r != "" {
				risk = r
			}
			return ResolvedPolicy{
				Policy: PolicyManualRequired,
				Origin: "frontmatter require_user",
				Source: OriginFrontmatter,
				Risk:   risk,
			}
		}
		if matchAny(call.ToolName, fm.AutoApprove) {
			return ResolvedPolicy{
				Policy: PolicyAlwaysAllowed,
				Origin: "frontmatter auto_approve",
				Source: OriginFrontmatter,
			}
		}
	}

	// Step 8: hardcoded default. The Manager passes its
	// DestructiveTools list when constructing the call's Frontmatter
	// pointer is nil; for now read from the Manager via a global is
	// awkward. We expose a Manager method DefaultPolicy(toolName)
	// that the Gate consults instead — see manager.go. PolicyStore
	// itself returns AlwaysAllowed here as the chain's natural
	// terminal.
	return ResolvedPolicy{
		Policy: PolicyAlwaysAllowed,
		Origin: "default",
		Source: OriginDefault,
	}
}

// Set upserts a policy row. Returns oldDecision (or "" when no row
// existed before), changed=true when the snapshot was actually
// modified, error on validation / DB failure.
//
// The caller (policy_set tool body) is responsible for emitting the
// policy_changed event using the returned values. Keeping event
// emission outside the store keeps the data layer pure.
func (p *PolicyStore) Set(ctx context.Context, pol Policy) (oldDecision string, changed bool, err error) {
	if err := validatePolicy(pol); err != nil {
		return "", false, err
	}
	if pol.AgentID == "" {
		pol.AgentID = p.agentID
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Read the current row directly from the DB so we don't race a
	// concurrent writer that landed since the cache was built. The
	// snapshot is a hot-cache for reads, NOT the source of truth on
	// the write path.
	prev, getErr := p.store.GetPolicy(ctx, pol.AgentID, pol.ToolName, pol.Scope)
	prevExists := getErr == nil
	switch {
	case getErr != nil && getErr != apstore.ErrPolicyNotFound:
		return "", false, fmt.Errorf("approvals/policy: read prev: %w", getErr)
	}
	if prevExists &&
		prev.Policy == pol.Decision.String() &&
		prev.Note == pol.Note {
		// Idempotent no-op — same decision + note. Don't touch the
		// row; don't emit an event.
		return prev.Policy, false, nil
	}

	rec := apstore.PolicyRecord{
		AgentID:   pol.AgentID,
		ToolName:  pol.ToolName,
		Scope:     pol.Scope,
		Policy:    pol.Decision.String(),
		Note:      pol.Note,
		CreatedBy: pol.CreatedBy,
	}
	if err := p.store.UpsertPolicy(ctx, rec); err != nil {
		return "", false, fmt.Errorf("approvals/policy: upsert: %w", err)
	}

	// Rebuild the snapshot from the DB to capture the new state.
	// One full reload is fine at this cardinality (≤ 100 rows) and
	// avoids subtle bugs if a parallel writer landed between the
	// upsert and the rebuild.
	rows, err := p.store.LoadAllPolicies(ctx)
	if err != nil {
		return "", false, fmt.Errorf("approvals/policy: reload after set: %w", err)
	}
	snap, err := buildSnapshot(rows)
	if err != nil {
		return "", false, err
	}
	p.snapshot.Store(snap)

	old := ""
	if prevExists {
		old = prev.Policy
	}
	return old, true, nil
}

// Remove deletes a policy row. Returns existed=true when a row was
// actually deleted, with oldDecision populated for the caller's
// event emission. Idempotent on missing rows.
func (p *PolicyStore) Remove(ctx context.Context, agentID, toolName, scope string) (oldDecision string, existed bool, err error) {
	if toolName == "" || scope == "" {
		return "", false, fmt.Errorf("%w: empty toolName or scope", ErrInvalidScope)
	}
	if agentID == "" {
		agentID = p.agentID
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	prev, getErr := p.store.GetPolicy(ctx, agentID, toolName, scope)
	if getErr == apstore.ErrPolicyNotFound {
		return "", false, nil
	}
	if getErr != nil {
		return "", false, fmt.Errorf("approvals/policy: read for remove: %w", getErr)
	}

	deleted, err := p.store.DeletePolicy(ctx, agentID, toolName, scope)
	if err != nil {
		return "", false, fmt.Errorf("approvals/policy: delete: %w", err)
	}
	if !deleted {
		return "", false, nil
	}

	rows, err := p.store.LoadAllPolicies(ctx)
	if err != nil {
		return "", false, fmt.Errorf("approvals/policy: reload after delete: %w", err)
	}
	snap, err := buildSnapshot(rows)
	if err != nil {
		return "", false, err
	}
	p.snapshot.Store(snap)

	return prev.Policy, true, nil
}

// List returns every policy row matching the optional scope/tool
// filters from the cache. Used by the policy_list tool. Empty scope
// + empty toolName → return everything.
func (p *PolicyStore) List(scope, toolName string) []ResolvedRow {
	snap := p.snapshot.Load()
	out := make([]ResolvedRow, 0, len(snap.exact)+len(snap.prefix))
	for _, r := range snap.exact {
		if scope != "" && r.Scope != scope {
			continue
		}
		if toolName != "" && r.ToolName != toolName {
			continue
		}
		out = append(out, r)
	}
	for _, e := range snap.prefix {
		if scope != "" && e.Scope != scope {
			continue
		}
		row := ResolvedRow{
			Scope:    e.Scope,
			ToolName: e.Prefix + "*",
			Decision: e.Decision,
		}
		if toolName != "" && row.ToolName != toolName {
			continue
		}
		out = append(out, row)
	}
	// Sort: scope priority, then tool name, for deterministic output.
	sort.Slice(out, func(i, j int) bool {
		pi, pj := scopePriority(out[i].Scope), scopePriority(out[j].Scope)
		if pi != pj {
			return pi < pj
		}
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		return out[i].ToolName < out[j].ToolName
	})
	return out
}

// ────────────────────────────────────────────────────────────────────
// snapshot internals
// ────────────────────────────────────────────────────────────────────

func buildSnapshot(rows []apstore.PolicyRecord) (*policySnapshot, error) {
	snap := &policySnapshot{
		exact:  make(map[snapshotKey]ResolvedRow, len(rows)),
		prefix: make([]prefixEntry, 0),
	}
	for _, r := range rows {
		dec, err := ParsePolicyDecision(r.Policy)
		if err != nil {
			// Skip malformed rows but keep the snapshot building —
			// otherwise one bad row would brick the cache.
			continue
		}
		if isStorablePrefixGlob(r.ToolName) {
			prefix := strings.TrimSuffix(r.ToolName, "*")
			snap.prefix = append(snap.prefix, prefixEntry{
				Scope:    r.Scope,
				Prefix:   prefix,
				Decision: dec,
				Origin:   fmt.Sprintf("%s prefix %s", r.Scope, r.ToolName),
			})
		} else {
			snap.exact[snapshotKey{Scope: r.Scope, ToolName: r.ToolName}] = ResolvedRow{
				Scope:     r.Scope,
				ToolName:  r.ToolName,
				Decision:  dec,
				Note:      r.Note,
				CreatedBy: r.CreatedBy,
				UpdatedAt: r.UpdatedAt,
			}
		}
	}
	// Sort prefix entries: scope priority ASC, then prefix length DESC
	// (longest match wins per scope).
	sort.Slice(snap.prefix, func(i, j int) bool {
		pi, pj := scopePriority(snap.prefix[i].Scope), scopePriority(snap.prefix[j].Scope)
		if pi != pj {
			return pi < pj
		}
		if snap.prefix[i].Scope != snap.prefix[j].Scope {
			return snap.prefix[i].Scope < snap.prefix[j].Scope
		}
		return len(snap.prefix[i].Prefix) > len(snap.prefix[j].Prefix)
	})
	return snap, nil
}

// prefixMatch returns the longest-prefix match for the given scope
// + tool name. Walks the sorted prefix slice; returns on first hit
// (because prefixes within a scope are pre-sorted by length DESC).
func (s *policySnapshot) prefixMatch(scope, toolName string) (prefixEntry, bool) {
	for _, e := range s.prefix {
		if e.Scope != scope {
			continue
		}
		if strings.HasPrefix(toolName, e.Prefix) {
			return e, true
		}
	}
	return prefixEntry{}, false
}

func resolvedFromRow(r ResolvedRow, origin string) ResolvedPolicy {
	rp := ResolvedPolicy{
		Policy: r.Decision,
		Origin: origin,
		Source: OriginCache,
	}
	if r.Decision == PolicyManualRequired {
		rp.Risk = RiskMedium
	}
	return rp
}

func resolvedFromEntry(e prefixEntry) ResolvedPolicy {
	rp := ResolvedPolicy{
		Policy: e.Decision,
		Origin: e.Origin,
		Source: OriginCache,
	}
	if e.Decision == PolicyManualRequired {
		rp.Risk = RiskMedium
	}
	return rp
}

// isPrefixGlob reports whether s ends in "*" — used by both
// frontmatter matchAny and the snapshot builder. The bare "*"
// matches anything.
func isPrefixGlob(s string) bool {
	return strings.HasSuffix(s, "*")
}

// isStorablePrefixGlob is the stricter check used at policy store
// time to reject malformed entries like a bare "*" that match
// anything (validatePolicy already refuses ToolName == "*", but
// the snapshot builder needs to refuse any zero-prefix glob too
// so a stale row can't accidentally widen to an everything-match).
func isStorablePrefixGlob(s string) bool {
	return len(s) >= 2 && strings.HasSuffix(s, "*")
}

// matchAny reports whether tool matches any pattern in patterns
// (exact name OR prefix glob ending in `*`). Used by the frontmatter
// step of Resolve.
func matchAny(tool string, patterns []string) bool {
	for _, p := range patterns {
		if p == tool {
			return true
		}
		if isPrefixGlob(p) {
			prefix := strings.TrimSuffix(p, "*")
			if strings.HasPrefix(tool, prefix) {
				return true
			}
		}
	}
	return false
}

// validatePolicy enforces the wire-level invariants on a Policy
// record before it lands in the store / cache.
func validatePolicy(p Policy) error {
	if p.ToolName == "" || p.ToolName == "*" {
		return fmt.Errorf("%w: tool_name", ErrInvalidToolName)
	}
	if !validScope(p.Scope) {
		return fmt.Errorf("%w: %q", ErrInvalidScope, p.Scope)
	}
	switch p.Decision {
	case PolicyAlwaysAllowed, PolicyManualRequired, PolicyDenied:
	default:
		return fmt.Errorf("%w: enum out of range", ErrInvalidPolicy)
	}
	return nil
}

func validScope(scope string) bool {
	if scope == "global" {
		return true
	}
	if strings.HasPrefix(scope, "skill:") && len(scope) > len("skill:") {
		return true
	}
	if strings.HasPrefix(scope, "role:") {
		// role:<skill>:<role> — must contain at least one ':' after "role:"
		rest := strings.TrimPrefix(scope, "role:")
		if i := strings.Index(rest, ":"); i > 0 && i < len(rest)-1 {
			return true
		}
	}
	return false
}
