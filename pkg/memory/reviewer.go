package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	learnstore "github.com/hugr-lab/hugen/pkg/memory/learning/store"
	memstore "github.com/hugr-lab/hugen/pkg/memory/store"
	"github.com/hugr-lab/hugen/pkg/models"
	"github.com/hugr-lab/hugen/pkg/scheduler"
	sessstore "github.com/hugr-lab/hugen/pkg/sessions/store"
	"github.com/hugr-lab/hugen/pkg/skills"
	"github.com/hugr-lab/query-engine/types"
	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

// Reviewer drives rolling-window session fact extraction. One Reviewer
// instance is shared across the scheduler (scheduler picks session IDs
// and calls Review). Unlike the earlier "one review per closed
// session" flow, the reviewer now processes windowed slices of
// ongoing and closed sessions, writing a review_checkpoint audit row
// after every successful window — session_reviews is no longer used as
// a cursor, since the same session can be reviewed many times.
type Reviewer struct {
	memory   *memstore.Client
	learning *learnstore.Client
	sessions *sessstore.Client
	router   *models.Router
	logger   *slog.Logger

	// Injected merged memory config — reviewer falls back to this
	// static agent-level config when no skills declare memory knobs
	// for a given session.
	config MergedConfig

	// Volatility-to-duration map from config.MemoryConfig. Falls back
	// to defaults when empty.
	volatility map[string]time.Duration

	// loadSkillMemory is the per-session skill-config fetcher. See
	// ReviewerOptions.LoadSkillMemory.
	loadSkillMemory func(ctx context.Context, skillName string) (*skills.SkillMemoryConfig, error)

	// dedupThreshold is the cosine-distance cutoff at which a newly
	// extracted fact is treated as a reinforcement of an existing one.
	dedupThreshold float64

	// tokens estimates per-event and per-window token counts so the
	// reviewer can honour skill-declared window_tokens/overlap_tokens.
	tokens *models.TokenEstimator

	// Agent-level defaults for the rolling-window knobs; each one is
	// overridden per-session by the merged skill config when that
	// skill sets a non-zero value.
	defaultWindowTokens  int
	defaultOverlapTokens int
	defaultFloorAge      time.Duration
	defaultExcludeTypes  []string

	// now is injectable for deterministic tests.
	now func() time.Time

	// sched: set by bindScheduler when the reviewer is registered as a
	// scheduler task. QueueReview uses it to nudge the scheduler.
	sched *scheduler.Scheduler

	recentlyClosedMu sync.Mutex
	recentlyClosed   map[string]time.Time
}

// ReviewerOptions bundle reviewer construction parameters. The
// reviewer builds its own memstore / learnstore / sessstore clients
// internally from Querier + AgentID + AgentShort.
type ReviewerOptions struct {
	Querier    types.Querier
	AgentID    string
	AgentShort string
	Router     *models.Router
	Logger     *slog.Logger
	Volatility map[string]time.Duration

	// LoadSkillMemory returns the per-skill memory config for a skill
	// by name. Typically wired to `pkg/skills.Manager.Load(ctx,
	// name).Memory`. When set, Review derives active skills from the
	// transcript's skill_loaded / skill_unloaded events, loads each
	// skill's memory config, and calls Merge. When nil, the reviewer
	// uses its static Config.
	LoadSkillMemory func(ctx context.Context, skillName string) (*skills.SkillMemoryConfig, error)

	// Tokens powers per-event token estimation used for rolling-window
	// composition. Required for the reviewer to honour WindowTokens /
	// OverlapTokens; callers should pass the same estimator used by
	// the compactor so calibration is shared.
	Tokens *models.TokenEstimator

	// Agent-level rolling-window defaults. Any field left at its zero
	// value is replaced by the hard-coded defaults at construction
	// time.
	DefaultWindowTokens  int
	DefaultOverlapTokens int
	DefaultFloorAge      time.Duration
	DefaultExcludeTypes  []string

	// Memory / Learning / Sessions are optional pre-built clients.
	// When set, the reviewer skips its internal New() calls.
	Memory   *memstore.Client
	Learning *learnstore.Client
	Sessions *sessstore.Client
}

// NewReviewer builds a Reviewer. Router is optional — if nil the
// reviewer refuses to run (returns ErrNoModel). Querier is required;
// the reviewer constructs its own store clients internally.
func NewReviewer(opts ReviewerOptions) (*Reviewer, error) {
	if opts.Querier == nil {
		return nil, fmt.Errorf("learning: Reviewer requires Querier")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	memC := opts.Memory
	if memC == nil {
		c, err := memstore.New(opts.Querier, memstore.Options{
			AgentID: opts.AgentID, AgentShort: opts.AgentShort, Logger: opts.Logger,
		})
		if err != nil {
			return nil, fmt.Errorf("learning: build memory store: %w", err)
		}
		memC = c
	}
	learnC := opts.Learning
	if learnC == nil {
		c, err := learnstore.New(opts.Querier, learnstore.Options{
			AgentID: opts.AgentID, AgentShort: opts.AgentShort, Logger: opts.Logger,
		})
		if err != nil {
			return nil, fmt.Errorf("learning: build learning store: %w", err)
		}
		learnC = c
	}
	sessC := opts.Sessions
	if sessC == nil {
		c, err := sessstore.New(opts.Querier, sessstore.Options{
			AgentID: opts.AgentID, AgentShort: opts.AgentShort, Logger: opts.Logger,
		})
		if err != nil {
			return nil, fmt.Errorf("learning: build sessions store: %w", err)
		}
		sessC = c
	}

	winTok := opts.DefaultWindowTokens
	if winTok <= 0 {
		winTok = 4000
	}
	overTok := opts.DefaultOverlapTokens
	if overTok <= 0 {
		overTok = 500
	}
	floor := opts.DefaultFloorAge
	if floor <= 0 {
		floor = 1 * time.Hour
	}
	exclude := opts.DefaultExcludeTypes
	if len(exclude) == 0 {
		exclude = []string{"compaction_summary", "reasoning", "error"}
	}

	return &Reviewer{
		memory:               memC,
		learning:             learnC,
		sessions:             sessC,
		router:               opts.Router,
		logger:               opts.Logger,
		volatility:           opts.Volatility,
		loadSkillMemory:      opts.LoadSkillMemory,
		tokens:               opts.Tokens,
		defaultWindowTokens:  winTok,
		defaultOverlapTokens: overTok,
		defaultFloorAge:      floor,
		defaultExcludeTypes:  exclude,
		dedupThreshold:       0.1, // cosine distance < 0.1 → duplicate (i.e. similarity > 0.9)
		now:                  func() time.Time { return time.Now().UTC() },
		recentlyClosed:       map[string]time.Time{},
	}, nil
}

// ReviewStats summarises the outcome of a single window review so the
// scheduler can persist the result as a review_checkpoint audit row.
type ReviewStats struct {
	FactsStored     int
	FactsReinforced int
	HypothesesAdded int
	Tokens          int  // estimated tokens of the filtered window
	SkippedBelowMin bool // true when the min_tool_calls gate fired
}

// Review runs the extraction pipeline on a windowed slice of a
// session's transcript. fromSeq <= 0 means "from the start of the
// session"; toSeq <= 0 means "through the latest event". The caller
// (Tick) picks the window; direct callers that want the whole session
// pass (0, -1).
//
// Unlike the earlier design, Review no longer consults session_reviews
// — the new checkpoint cursor lives in memory_log event_type
// "review_checkpoint" because a session can be reviewed many times.
func (r *Reviewer) Review(ctx context.Context, sessionID string, fromSeq, toSeq int) (ReviewStats, error) {
	var stats ReviewStats
	if sessionID == "" {
		return stats, fmt.Errorf("learning: Reviewer: empty sessionID")
	}

	allEvents, err := r.sessions.GetEventsFull(ctx, sessionID)
	if err != nil {
		return stats, fmt.Errorf("learning: GetEventsFull: %w", err)
	}

	// Derive per-session merged config from the FULL transcript so the
	// set of active skills reflects everything up to the end of the
	// window (we don't want to drop skill config from events outside
	// [from, to]).
	merged := r.mergedConfigFor(ctx, allEvents)

	// Apply window bounds.
	var windowed []sessstore.EventFull
	for _, ev := range allEvents {
		if fromSeq > 0 && ev.Seq < fromSeq {
			continue
		}
		if toSeq > 0 && ev.Seq > toSeq {
			continue
		}
		windowed = append(windowed, ev)
	}
	if len(windowed) == 0 {
		return stats, nil
	}

	// Filter out excluded event types.
	excluded := r.excludeTypes(merged)
	filtered := make([]sessstore.EventFull, 0, len(windowed))
	for _, ev := range windowed {
		if _, skip := excluded[ev.EventType]; skip {
			continue
		}
		filtered = append(filtered, ev)
	}
	if len(filtered) == 0 {
		return stats, nil
	}

	// Count tool_calls over the WINDOW only — not the whole session.
	toolCalls := 0
	for _, ev := range filtered {
		if ev.EventType == "tool_call" {
			toolCalls++
		}
	}
	minCalls := 1
	if merged.MinToolCalls > 0 {
		minCalls = merged.MinToolCalls
	}

	// Estimate tokens of the filtered window.
	for _, ev := range filtered {
		stats.Tokens += r.estimateEventTokens(ev)
	}

	if toolCalls < minCalls {
		stats.SkippedBelowMin = true
		r.logger.Info("reviewer: skipping window below min_tool_calls",
			"session", sessionID, "tool_calls", toolCalls, "min", minCalls,
			"from_seq", fromSeq, "to_seq", toSeq)
		return stats, nil
	}

	if r.router == nil {
		return stats, fmt.Errorf("learning: Reviewer: no router")
	}

	notes, _ := r.sessions.ListNotes(ctx, sessionID) // best-effort; notes are optional

	// Fetch prior facts produced by earlier reviews of this session so
	// the prompt can tell the LLM which extractions are already on
	// record (dedup hint → fewer duplicates, more reinforcements).
	priorFacts := r.collectPriorFacts(ctx, sessionID)

	prompt := r.buildPrompt(filtered, notes, merged, priorFacts)

	llm := r.router.ModelFor(models.IntentSummarization)
	rawOutput, _, err := RunOnce(ctx, llm, prompt)
	if err != nil {
		return stats, fmt.Errorf("learning: LLM: %w", err)
	}

	parsed, err := parseReviewOutput(rawOutput)
	if err != nil {
		return stats, fmt.Errorf("learning: parse: %w", err)
	}

	for _, f := range parsed.Facts {
		reinforced, err := r.upsertFact(ctx, sessionID, f, merged)
		if err != nil {
			r.logger.Warn("reviewer: upsert fact failed",
				"session", sessionID, "content", f.Content, "err", err)
			continue
		}
		if reinforced {
			stats.FactsReinforced++
		} else {
			stats.FactsStored++
		}
	}

	for _, hy := range parsed.Hypotheses {
		if _, err := r.learning.CreateHypothesis(ctx, learnstore.Hypothesis{
			Content:        hy.Content,
			Category:       hy.Category,
			Priority:       defaultStr(hy.Priority, "medium"),
			Verification:   hy.Verification,
			EstimatedCalls: hy.EstimatedCalls,
			SourceSession:  sessionID,
		}); err != nil {
			r.logger.Warn("reviewer: create hypothesis failed",
				"session", sessionID, "err", err)
			continue
		}
		stats.HypothesesAdded++
	}

	return stats, nil
}

// priorFactEntry is a minimal view of a previously extracted fact used
// to render the "Already extracted" section of the prompt.
type priorFactEntry struct {
	Content  string
	Category string
}

// collectPriorFacts returns facts produced by earlier reviews of this
// session (memory_log event_type = "store" rows) so the prompt can
// dedup against them. Best-effort: errors return an empty slice.
func (r *Reviewer) collectPriorFacts(ctx context.Context, sessionID string) []priorFactEntry {
	logs, err := r.memory.ListLog(ctx, memstore.ListLogOpts{
		EventType: "store",
		SessionID: sessionID,
		Limit:     50,
	})
	if err != nil {
		return nil
	}
	out := make([]priorFactEntry, 0, len(logs))
	seen := map[string]struct{}{}
	for _, l := range logs {
		if l.MemoryItemID == "" {
			continue
		}
		if _, dup := seen[l.MemoryItemID]; dup {
			continue
		}
		seen[l.MemoryItemID] = struct{}{}
		item, err := r.memory.Get(ctx, l.MemoryItemID)
		if err != nil || item == nil {
			continue
		}
		out = append(out, priorFactEntry{Content: item.Content, Category: item.Category})
	}
	return out
}

// upsertFact stores a new fact or reinforces a close duplicate.
// Returns reinforced=true when a duplicate was found. Dedup is a
// simple content-substring match until embeddings are wired into the
// reviewer — keeps the path testable without an embedding model.
func (r *Reviewer) upsertFact(ctx context.Context, sessionID string, f extractedFact, merged MergedConfig) (bool, error) {
	// Look for near-duplicates by category + keyword.
	existing, _ := r.memory.Search(ctx, f.Content, nil, memstore.SearchOpts{
		Category: f.Category,
		Limit:    5,
	})
	for _, cand := range existing {
		if equalEnough(cand.Content, f.Content) {
			// Reinforce with modest score bonus.
			bonus := 0.15
			if err := r.memory.Reinforce(ctx, cand.ID, bonus, f.Tags, linkList(f.Links)); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	// Compute validity window from volatility.
	vol := defaultStr(f.Volatility, "stable")
	dur := r.durationFor(vol)
	now := r.now()
	item := memstore.Item{
		Content:    f.Content,
		Category:   f.Category,
		Volatility: vol,
		Score:      r.initialScoreFor(f.Category, merged),
		Source:     "review:" + sessionID,
		ValidFrom:  now,
		ValidTo:    now.Add(dur),
	}
	_, err := r.memory.Store(ctx, item, f.Tags, linkList(f.Links))
	return false, err
}

// buildPrompt assembles the summarisation prompt from transcript +
// notes + merged review prompt. Includes two dedup/guidance sections:
// a list of already-extracted facts for this session (so the LLM
// reinforces rather than re-emits) and the whitelist of allowed
// categories drawn from the merged skill configs.
func (r *Reviewer) buildPrompt(events []sessstore.EventFull, notes []sessstore.Note, merged MergedConfig, priorFacts []priorFactEntry) string {
	var sb strings.Builder
	sb.WriteString(r.reviewPrompt(merged))

	// Category whitelist. Sorted alphabetically for stable prompts.
	if len(merged.Categories) > 0 {
		keys := make([]string, 0, len(merged.Categories))
		for k := range merged.Categories {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		sb.WriteString("\n\n## Allowed categories\n")
		for _, k := range keys {
			sb.WriteString("- ")
			sb.WriteString(k)
			if desc := merged.Categories[k].Description; desc != "" {
				sb.WriteString(" — ")
				sb.WriteString(desc)
			}
			sb.WriteString("\n")
		}
	}

	// Already-extracted facts.
	if len(priorFacts) > 0 {
		sb.WriteString("\n## Already extracted from this session\n")
		for _, pf := range priorFacts {
			sb.WriteString("- ")
			sb.WriteString(pf.Content)
			if pf.Category != "" {
				sb.WriteString(" [")
				sb.WriteString(pf.Category)
				sb.WriteString("]")
			}
			sb.WriteString("\n")
		}
		sb.WriteString("Do NOT repeat these; use Reinforce (same content phrased differently with confirming evidence) only if you see new corroboration.\n")
	}

	sb.WriteString("\n## Transcript\n")
	for _, ev := range events {
		switch ev.EventType {
		case "user_message":
			sb.WriteString("USER: ")
			sb.WriteString(ev.Content)
		case "llm_response":
			sb.WriteString("AGENT: ")
			sb.WriteString(ev.Content)
		case "tool_call":
			sb.WriteString("TOOL_CALL: ")
			sb.WriteString(ev.ToolName)
			if len(ev.ToolArgs) > 0 {
				b, _ := json.Marshal(ev.ToolArgs)
				sb.WriteString(" ")
				sb.Write(b)
			}
		case "tool_result":
			sb.WriteString("TOOL_RESULT: ")
			sb.WriteString(ev.ToolName)
			sb.WriteString(" → ")
			sb.WriteString(truncate(ev.ToolResult, 1000))
		default:
			continue
		}
		sb.WriteString("\n")
	}
	if len(notes) > 0 {
		sb.WriteString("\n## Session notes\n")
		for _, n := range notes {
			sb.WriteString("- ")
			sb.WriteString(n.Content)
			sb.WriteString("\n")
		}
	}
	sb.WriteString(`
## Output format
Reply with a single JSON object:
{
  "facts": [{"content": "...", "category": "...", "volatility": "stable|slow|moderate|fast|volatile", "tags": ["..."], "links": ["..."]}],
  "hypotheses": [{"content": "...", "category": "...", "priority": "high|medium|low", "verification": "...", "estimated_calls": 3}]
}`)
	return sb.String()
}

func (r *Reviewer) reviewPrompt(merged MergedConfig) string {
	if merged.ReviewPrompt != "" {
		return merged.ReviewPrompt
	}
	if r.config.ReviewPrompt != "" {
		return r.config.ReviewPrompt
	}
	return `You are reviewing a completed agent session. Extract durable facts worth remembering (schema structures, working query templates, anti-patterns, user preferences) and testable hypotheses you noticed but did not verify. Skip chitchat, retries, and one-off noise.`
}

// mergedConfigFor builds the per-session merged config by replaying
// skill_loaded / skill_unloaded events, then asking loadSkillMemory
// for each active skill's memory.yaml. Falls back to the reviewer's
// static config when no loader is wired.
func (r *Reviewer) mergedConfigFor(ctx context.Context, events []sessstore.EventFull) MergedConfig {
	if r.loadSkillMemory == nil {
		return r.config
	}
	active := map[string]struct{}{}
	for _, ev := range events {
		switch ev.EventType {
		case sessstore.EventTypeSkillLoaded:
			if name := skillNameFromEvent(ev); name != "" {
				active[name] = struct{}{}
			}
		case sessstore.EventTypeSkillUnloaded:
			delete(active, skillNameFromEvent(ev))
		}
	}
	if len(active) == 0 {
		return r.config
	}
	configs := make([]NamedConfig, 0, len(active))
	for name := range active {
		cfg, err := r.loadSkillMemory(ctx, name)
		if err != nil {
			r.logger.Warn("reviewer: load skill memory", "skill", name, "err", err)
			continue
		}
		configs = append(configs, NamedConfig{Name: name, Config: cfg})
	}
	return MergeWithLogger(configs, r.logger)
}

func skillNameFromEvent(ev sessstore.EventFull) string {
	if ev.Metadata != nil {
		if name, ok := ev.Metadata["skill"].(string); ok && name != "" {
			return name
		}
	}
	return ev.Content
}

func (r *Reviewer) durationFor(volatility string) time.Duration {
	if r.volatility != nil {
		if d, ok := r.volatility[volatility]; ok && d > 0 {
			return d
		}
	}
	switch volatility {
	case "volatile":
		return 24 * time.Hour
	case "fast":
		return 7 * 24 * time.Hour
	case "moderate":
		return 30 * 24 * time.Hour
	case "slow":
		return 90 * 24 * time.Hour
	}
	return 365 * 24 * time.Hour // stable default
}

func (r *Reviewer) initialScoreFor(category string, merged MergedConfig) float64 {
	if merged.Categories != nil {
		if cat, ok := merged.Categories[category]; ok && cat.InitialScore > 0 {
			return cat.InitialScore
		}
	}
	if r.config.Categories != nil {
		if cat, ok := r.config.Categories[category]; ok && cat.InitialScore > 0 {
			return cat.InitialScore
		}
	}
	return 0.5
}

// ------------------------------------------------------------
// Rolling-window helpers
// ------------------------------------------------------------

func (r *Reviewer) floorAge(merged MergedConfig) time.Duration {
	if merged.FloorAge > 0 {
		return merged.FloorAge
	}
	return r.defaultFloorAge
}

func (r *Reviewer) windowTokens(merged MergedConfig) int {
	if merged.WindowTokens > 0 {
		return merged.WindowTokens
	}
	return r.defaultWindowTokens
}

func (r *Reviewer) overlapTokens(merged MergedConfig) int {
	if merged.OverlapTokens > 0 {
		return merged.OverlapTokens
	}
	return r.defaultOverlapTokens
}

// excludeTypes returns the union of the merged config's exclude set
// and the reviewer's default list. Always returns a non-nil map.
func (r *Reviewer) excludeTypes(merged MergedConfig) map[string]struct{} {
	out := make(map[string]struct{}, len(merged.ExcludeEventTypes)+len(r.defaultExcludeTypes))
	for k := range merged.ExcludeEventTypes {
		if k != "" {
			out[k] = struct{}{}
		}
	}
	for _, k := range r.defaultExcludeTypes {
		if k != "" {
			out[k] = struct{}{}
		}
	}
	return out
}

// estimateEventTokens approximates the token cost of one session
// event for rolling-window composition. Combines every text field the
// prompt is going to stringify so the budget tracks prompt size.
func (r *Reviewer) estimateEventTokens(ev sessstore.EventFull) int {
	if r.tokens == nil {
		// Fallback: char-count / 4 heuristic.
		n := len(ev.Author) + len(ev.Content) + len(ev.ToolName) + len(ev.ToolResult)
		if len(ev.ToolArgs) > 0 {
			if b, err := json.Marshal(ev.ToolArgs); err == nil {
				n += len(b)
			}
		}
		return n / 4
	}
	var sb strings.Builder
	sb.WriteString(ev.Author)
	sb.WriteByte(' ')
	sb.WriteString(ev.Content)
	sb.WriteByte(' ')
	sb.WriteString(ev.ToolName)
	if len(ev.ToolArgs) > 0 {
		if b, err := json.Marshal(ev.ToolArgs); err == nil {
			sb.WriteByte(' ')
			sb.Write(b)
		}
	}
	sb.WriteByte(' ')
	sb.WriteString(ev.ToolResult)
	return r.tokens.Estimate(sb.String())
}

// walkTokensForward walks events AFTER startSeq (events.Seq > startSeq),
// accumulating tokens until budget is reached. Returns the largest seq
// included and the actual token count used. When no event is past
// startSeq, returns (startSeq, 0).
func (r *Reviewer) walkTokensForward(events []sessstore.EventFull, startSeq int, budget int) (endSeq int, tokensUsed int) {
	endSeq = startSeq
	for _, ev := range events {
		if ev.Seq <= startSeq {
			continue
		}
		tokensUsed += r.estimateEventTokens(ev)
		endSeq = ev.Seq
		if budget > 0 && tokensUsed >= budget {
			break
		}
	}
	return endSeq, tokensUsed
}

// walkTokensBackward walks events BEFORE startSeq+1 in reverse,
// accumulating tokens until budget is reached. Returns the smallest
// seq included (the "from" seq of the overlap band). When no event
// precedes startSeq, returns startSeq+1 (no overlap).
func (r *Reviewer) walkTokensBackward(events []sessstore.EventFull, startSeq int, budget int) int {
	if budget <= 0 || startSeq <= 0 {
		return startSeq + 1
	}
	used := 0
	from := startSeq + 1
	// Iterate in reverse through events at seq <= startSeq.
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.Seq > startSeq {
			continue
		}
		used += r.estimateEventTokens(ev)
		from = ev.Seq
		if used >= budget {
			break
		}
	}
	return from
}

// ------------------------------------------------------------
// JSON parsing
// ------------------------------------------------------------

type extractedFact struct {
	Content    string   `json:"content"`
	Category   string   `json:"category"`
	Volatility string   `json:"volatility"`
	Tags       []string `json:"tags"`
	Links      []string `json:"links"`
}

type extractedHypothesis struct {
	Content        string `json:"content"`
	Category       string `json:"category"`
	Priority       string `json:"priority"`
	Verification   string `json:"verification"`
	EstimatedCalls int    `json:"estimated_calls"`
}

type reviewOutput struct {
	Facts      []extractedFact       `json:"facts"`
	Hypotheses []extractedHypothesis `json:"hypotheses"`
}

func parseReviewOutput(raw string) (reviewOutput, error) {
	raw = strings.TrimSpace(raw)
	// Strip markdown code fences if present.
	if strings.HasPrefix(raw, "```") {
		if idx := strings.Index(raw, "\n"); idx >= 0 {
			raw = raw[idx+1:]
		}
		raw = strings.TrimSuffix(strings.TrimSpace(raw), "```")
	}
	// Find the outermost {...} span.
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end < 0 || end <= start {
		return reviewOutput{}, fmt.Errorf("no JSON object in output")
	}
	var out reviewOutput
	if err := json.Unmarshal([]byte(raw[start:end+1]), &out); err != nil {
		return reviewOutput{}, err
	}
	return out, nil
}

// ------------------------------------------------------------
// helpers
// ------------------------------------------------------------

func RunOnce(ctx context.Context, llm model.LLM, prompt string) (string, int, error) {
	req := &model.LLMRequest{
		Contents: []*genai.Content{{
			Role:  "user",
			Parts: []*genai.Part{{Text: prompt}},
		}},
	}
	var out strings.Builder
	var totalTokens int
	var seq iter.Seq2[*model.LLMResponse, error] = llm.GenerateContent(ctx, req, false)
	for resp, err := range seq {
		if err != nil {
			return "", totalTokens, err
		}
		if resp == nil {
			continue
		}
		if resp.Content != nil {
			for _, p := range resp.Content.Parts {
				if p != nil && p.Text != "" {
					out.WriteString(p.Text)
				}
			}
		}
		if resp.UsageMetadata != nil {
			totalTokens = int(resp.UsageMetadata.TotalTokenCount)
		}
		if resp.TurnComplete {
			break
		}
	}
	return out.String(), totalTokens, nil
}

func equalEnough(a, b string) bool {
	// Crude similarity fallback: lowercased exact match or one being a
	// prefix of the other within 20% length tolerance. Replace with
	// embedding cosine ≥ 0.9 once the reviewer uses embeddings.
	aa := strings.ToLower(strings.TrimSpace(a))
	bb := strings.ToLower(strings.TrimSpace(b))
	if aa == bb {
		return true
	}
	if len(aa) == 0 || len(bb) == 0 {
		return false
	}
	if len(aa) < len(bb) {
		aa, bb = bb, aa
	}
	if !strings.HasPrefix(aa, bb) {
		return false
	}
	return float64(len(bb))/float64(len(aa)) >= 0.8
}

func defaultStr(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func linkList(targetIDs []string) []memstore.Link {
	if len(targetIDs) == 0 {
		return nil
	}
	links := make([]memstore.Link, 0, len(targetIDs))
	for _, id := range targetIDs {
		links = append(links, memstore.Link{TargetID: id, Relation: "related"})
	}
	return links
}

// ------------------------------------------------------------
// Scheduler integration
// ------------------------------------------------------------

// reviewTaskName is the name the Reviewer uses when registering its
// Tick with the scheduler. Kept package-private so the string lives
// in one place (bindScheduler callers + Wake callers).
const reviewTaskName = "memory.review"

// maxPerTick caps the number of candidate sessions processed in a
// single Tick so a large backlog can't block the scheduler.
const maxPerTick = 5

// bindScheduler attaches the reviewer to a running scheduler so that
// QueueReview can Wake the matching task. Typically called by
// memory.Register; not part of the public Reviewer API. The delay
// parameter is retained for call-site compatibility but is now a
// no-op — age gating lives entirely inside Tick (floor_age per
// merged config).
func (r *Reviewer) bindScheduler(sched *scheduler.Scheduler, _ time.Duration) {
	r.sched = sched
}

// QueueReview implements sessions.ReviewQueuer. Called by
// sessions.Manager.Delete when a session closes. Records the session
// as "recently closed" so the next Tick treats it as ready
// regardless of floor_age, and nudges the scheduler.
func (r *Reviewer) QueueReview(sessionID string) {
	if sessionID == "" {
		return
	}
	now := r.nowFn()
	r.recentlyClosedMu.Lock()
	if r.recentlyClosed == nil {
		r.recentlyClosed = map[string]time.Time{}
	}
	r.recentlyClosed[sessionID] = now
	r.recentlyClosedMu.Unlock()

	if r.sched != nil {
		r.sched.Wake(reviewTaskName)
	}
}

// cursor captures the most recent review_checkpoint row for one
// session: EndSeq is the last event covered by the previous review;
// EventTime is the timestamp of that checkpoint (zero when unknown).
type cursor struct {
	EndSeq    int
	EventTime time.Time
}

// Tick is the Task registered with scheduler.Every("memory.review",…).
// It picks candidate sessions from (recentlyClosed snapshot ∪ active
// sessions), loads review_checkpoint cursors, decides readiness via
// the floor-age policy, and calls Review on up to maxPerTick windows.
func (r *Reviewer) Tick(ctx context.Context) error {
	// Snapshot and clear the recently-closed set atomically.
	r.recentlyClosedMu.Lock()
	closed := r.recentlyClosed
	r.recentlyClosed = map[string]time.Time{}
	r.recentlyClosedMu.Unlock()

	// Load review_checkpoint cursors. ListLog returns most-recent
	// first, so we keep only the first seen per sessionID.
	cursors := map[string]cursor{}
	logs, err := r.memory.ListLog(ctx, memstore.ListLogOpts{
		EventType: "review_checkpoint",
		Limit:     200,
	})
	if err != nil {
		r.logger.Warn("reviewer: list checkpoints", "err", err)
	}
	for _, l := range logs {
		if _, seen := cursors[l.SessionID]; seen {
			continue
		}
		end := 0
		if l.Details != nil {
			if v, ok := l.Details["end_seq"]; ok {
				end = asInt(v)
			}
		}
		cursors[l.SessionID] = cursor{EndSeq: end, EventTime: l.EventTime}
	}

	// Build candidate list: closed first, then active sessions not
	// already in the closed set.
	type candidate struct {
		sid    string
		closed bool
		status string
	}
	candidates := make([]candidate, 0, len(closed)+8)
	for sid := range closed {
		candidates = append(candidates, candidate{sid: sid, closed: true, status: "completed"})
	}
	active, err := r.sessions.ListActiveSessions(ctx)
	if err != nil {
		r.logger.Warn("reviewer: list active sessions", "err", err)
	}
	for _, rec := range active {
		if _, already := closed[rec.ID]; already {
			continue
		}
		candidates = append(candidates, candidate{sid: rec.ID, closed: false, status: rec.Status})
	}

	now := r.nowFn()
	type ready struct {
		sid    string
		closed bool
		events []sessstore.EventFull
		maxSeq int
		merged MergedConfig
		cur    cursor
		rec    *sessstore.Record
	}
	ready2 := make([]ready, 0, len(candidates))

	// First pass: decide readiness.
	for _, c := range candidates {
		events, ferr := r.sessions.GetEventsFull(ctx, c.sid)
		if ferr != nil {
			r.logger.Warn("reviewer: load events", "session", c.sid, "err", ferr)
			continue
		}
		if len(events) == 0 {
			continue
		}
		maxSeq := 0
		for _, ev := range events {
			if ev.Seq > maxSeq {
				maxSeq = ev.Seq
			}
		}
		cur := cursors[c.sid]
		if cur.EndSeq >= maxSeq {
			continue // nothing new since last checkpoint
		}
		isClosed := c.closed || c.status == "completed"
		merged := r.mergedConfigFor(ctx, events)
		floor := r.floorAge(merged)

		var rec *sessstore.Record
		if !isClosed {
			for i := range active {
				if active[i].ID == c.sid {
					rec = &active[i]
					break
				}
			}
		}

		passes := false
		switch {
		case isClosed:
			passes = true
		case !cur.EventTime.IsZero():
			if now.Sub(cur.EventTime) >= floor && cur.EndSeq < maxSeq {
				passes = true
			}
		case rec != nil:
			if now.Sub(rec.CreatedAt) >= floor && maxSeq > 0 {
				passes = true
			}
		}
		if !passes {
			continue
		}
		ready2 = append(ready2, ready{
			sid:    c.sid,
			closed: isClosed,
			events: events,
			maxSeq: maxSeq,
			merged: merged,
			cur:    cur,
			rec:    rec,
		})
		if len(ready2) >= maxPerTick {
			break
		}
	}

	// Second pass: process ready candidates.
	for _, rd := range ready2 {
		trail := 0
		if !rd.closed {
			trail = 2 // don't touch events right at the tail of an active session
		}
		to := rd.maxSeq - trail
		if to <= rd.cur.EndSeq {
			continue
		}
		start := rd.cur.EndSeq

		// Cap the window by windowTokens.
		walkedEnd, _ := r.walkTokensForward(rd.events, start, r.windowTokens(rd.merged))
		if walkedEnd > 0 && walkedEnd < to {
			to = walkedEnd
		}

		// Overlap prefix — same overlapTokens budget for active and
		// closed sessions (closed windows are usually small anyway).
		from := start + 1
		if start > 0 {
			from = max(1, r.walkTokensBackward(rd.events, start, r.overlapTokens(rd.merged)))
		}

		if to < from {
			continue
		}

		stats, rerr := r.Review(ctx, rd.sid, from, to)
		if rerr != nil {
			r.logger.Warn("memory.review: window review failed",
				"session", rd.sid, "from_seq", from, "to_seq", to, "err", rerr)
			continue
		}

		_ = r.memory.Log(ctx, memstore.LogEntry{
			EventType: "review_checkpoint",
			SessionID: rd.sid,
			AgentID:   r.memory.AgentID(),
			Details: map[string]any{
				"start_seq":        from,
				"end_seq":          to,
				"tokens":           stats.Tokens,
				"facts_stored":     stats.FactsStored,
				"facts_reinforced": stats.FactsReinforced,
				"hypotheses_added": stats.HypothesesAdded,
				"skipped":          stats.SkippedBelowMin,
			},
		})
	}

	return nil
}

// asInt coerces JSON-decoded numbers (float64 / json.Number / int) into
// int. Returns 0 on any non-numeric value.
func asInt(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int32:
		return int(t)
	case int64:
		return int(t)
	case float32:
		return int(t)
	case float64:
		return int(t)
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return int(i)
		}
	}
	return 0
}

// nowFn returns the reviewer's now() function; falls back to
// time.Now().UTC() when unset so QueueReview is safe to call without
// explicit test wiring.
func (r *Reviewer) nowFn() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now().UTC()
}
