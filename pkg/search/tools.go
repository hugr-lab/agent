package search

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"

	"github.com/hugr-lab/hugen/pkg/store/queries"
	"github.com/hugr-lab/hugen/pkg/tools"
	"github.com/hugr-lab/query-engine/types"
)

// ─────────────────────────────────────────────────────────────────
// session_context  (turn = open to all; mission = open to all;
//                   session/user = coordinator-only)
// ─────────────────────────────────────────────────────────────────

type sessionContextTool struct {
	s *Service
}

func (t *sessionContextTool) Name() string { return "session_context" }

func (t *sessionContextTool) Description() string {
	return "Retrieve events from your session corpus, ranked by relevance × recency. Required `scope`: turn (your last N events, no ranking) | mission (your own session, ranked) | session (coordinator + all sub-agent sessions, coord-only) | user (all your past root sessions, coord-only). Optional `query` (semantic search; rejected for scope=turn). Optional `last_n` (default 20), `half_life` (e.g. \"1h\", \"24h\", \"168h\" — defaults scale with scope), `date_from` / `date_to` (RFC3339 timestamps clamping the corpus). Returns hits with provenance (session_id + created_at) so you can quote results with citations."
}

func (t *sessionContextTool) IsLongRunning() bool { return false }

func (t *sessionContextTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: "OBJECT",
			Properties: map[string]*genai.Schema{
				"scope": {
					Type:        "STRING",
					Description: "Required. turn | mission | session | user.",
					Enum:        []string{"turn", "mission", "session", "user"},
				},
				"query": {
					Type:        "STRING",
					Description: "Optional semantic query. Required for ranked scopes when you want relevance × recency; omit for recency-only ordering. Rejected for scope=turn.",
				},
				"last_n": {
					Type:        "INTEGER",
					Description: "Max results. Default 20, hard cap 200. For scope=turn this is the tail length.",
				},
				"half_life": {
					Type:        "STRING",
					Description: "Recency half-life override (Go duration string, e.g. '1h', '24h', '168h'). Default scales with scope: mission=1h, session=24h, user=168h.",
				},
				"date_from": {
					Type:        "STRING",
					Description: "Optional RFC3339 lower bound on event created_at.",
				},
				"date_to": {
					Type:        "STRING",
					Description: "Optional RFC3339 upper bound on event created_at.",
				},
			},
			Required: []string{"scope"},
		},
	}
}

func (t *sessionContextTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *sessionContextTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	m, ok := args.(map[string]any)
	if !ok {
		return errEnvelope("session_context", fmt.Errorf("unexpected args type %T", args), "invalid_args")
	}

	scope := Scope(stringArg(m, "scope"))
	if err := scope.Validate(); err != nil {
		return errEnvelope("session_context", err, "invalid_scope")
	}
	query := stringArg(m, "query")
	if scope == ScopeTurn && query != "" {
		return errEnvelope("session_context", ErrTurnRejectsQuery, "turn_rejects_query")
	}

	lastN := t.s.defaultLimit
	if v, ok := m["last_n"].(float64); ok && v > 0 {
		lastN = int(v)
	} else if v, ok := m["last_n"].(int); ok && v > 0 {
		lastN = v
	}
	if lastN > 200 {
		lastN = 200
	}

	dateFrom := stringArg(m, "date_from")
	dateTo := stringArg(m, "date_to")
	if dateFrom != "" && dateTo != "" && dateFrom > dateTo {
		return errEnvelope("session_context", ErrInvalidDateRange, "invalid_date_range")
	}

	halfLife := t.s.halfLifeFor(scope)
	if hl := stringArg(m, "half_life"); hl != "" {
		parsed, err := time.ParseDuration(hl)
		if err != nil {
			return errEnvelope("session_context", ErrUnparseableHalfLife, "unparseable_half_life")
		}
		halfLife = parsed
	}

	stdCtx := toolCtxAsContext(ctx)
	callerSessionID := ctx.SessionID()

	// Sub-agent authorization for coord-only scopes.
	if scope.IsCoordinatorOnly() {
		rec, err := t.s.sessR.GetSession(stdCtx, callerSessionID)
		if err == nil && rec != nil && rec.SessionType != "root" {
			return errEnvelope("session_context", ErrScopeForbidden, "scope_forbidden")
		}
	}

	corpus, err := t.s.resolveCorpus(stdCtx, scope, callerSessionID, dateFrom, dateTo)
	if err != nil {
		if errors.Is(err, ErrInvalidScope) {
			return errEnvelope("session_context", err, "invalid_scope")
		}
		return errEnvelope("session_context", err, "internal_error")
	}
	if len(corpus.SessionIDs) == 0 {
		return map[string]any{
			"ok":                true,
			"hits":              []map[string]any{},
			"strategy":          string(StrategyRecency),
			"total_corpus_size": 0,
			"scope":             string(scope),
			"half_life":         halfLife.String(),
		}, nil
	}

	switch scope {
	case ScopeTurn:
		hits, err := t.s.runTailQuery(stdCtx, callerSessionID, lastN, dateFrom, dateTo)
		if err != nil {
			return errEnvelope("session_context", err, "internal_error")
		}
		return packResult(hits, StrategyRecency, scope, halfLife, len(hits)), nil
	default:
		strategy := StrategyRecency
		var hits []SearchHit
		switch {
		case query != "" && t.s.embedderEnabled:
			hits, err = t.s.runSemantic(stdCtx, corpus.SessionIDs, query, lastN, dateFrom, dateTo)
			if err != nil {
				return errEnvelope("session_context", err, "internal_error")
			}
			strategy = StrategySemantic
			if len(hits) == 0 {
				// Fallback to keyword ILIKE.
				hits, err = t.s.runKeyword(stdCtx, corpus.SessionIDs, query, lastN, dateFrom, dateTo)
				if err != nil {
					return errEnvelope("session_context", err, "internal_error")
				}
				if len(hits) > 0 {
					strategy = StrategyKeyword
				}
			}
		case query != "":
			// Embedder disabled — go straight to keyword.
			hits, err = t.s.runKeyword(stdCtx, corpus.SessionIDs, query, lastN, dateFrom, dateTo)
			if err != nil {
				return errEnvelope("session_context", err, "internal_error")
			}
			strategy = StrategyKeyword
		default:
			// No query — recency-only ordering.
			hits, err = t.s.runRecencyOnly(stdCtx, corpus.SessionIDs, lastN, dateFrom, dateTo)
			if err != nil {
				return errEnvelope("session_context", err, "internal_error")
			}
		}
		now := t.s.nowFn()
		if strategy == StrategySemantic {
			hits = applyRecencyRerank(hits, halfLife, now, lastN)
		} else {
			hits = applyRecencyOnly(hits, halfLife, now, lastN)
		}
		return packResult(hits, strategy, scope, halfLife, len(hits)), nil
	}
}

// ─────────────────────────────────────────────────────────────────
// session_events  (raw audit access)
// ─────────────────────────────────────────────────────────────────

type sessionEventsTool struct {
	s *Service
}

func (t *sessionEventsTool) Name() string { return "session_events" }

func (t *sessionEventsTool) Description() string {
	return "Raw structured access to one session's events. Audit/debug path — distinct from session_context (no ranking, no semantic). Filters: tool_name, author, event_type. Default limit 50, hard cap 500. order: asc | desc by seq (default asc)."
}

func (t *sessionEventsTool) IsLongRunning() bool { return false }

func (t *sessionEventsTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: "OBJECT",
			Properties: map[string]*genai.Schema{
				"session_id": {Type: "STRING", Description: "Session id to read."},
				"tool_name":  {Type: "STRING", Description: "Optional tool_name filter."},
				"author":     {Type: "STRING", Description: "Optional author filter (user | agent | subagent | system)."},
				"event_type": {Type: "STRING", Description: "Optional event_type filter (tool_result | llm_response | …)."},
				"limit":      {Type: "INTEGER", Description: "Max rows. Default 50, hard cap 500."},
				"order":      {Type: "STRING", Enum: []string{"asc", "desc"}, Description: "Ordering by seq. Default asc."},
			},
			Required: []string{"session_id"},
		},
	}
}

func (t *sessionEventsTool) ProcessRequest(_ tool.Context, req *model.LLMRequest) error {
	tools.Pack(req, t)
	return nil
}

func (t *sessionEventsTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	m, ok := args.(map[string]any)
	if !ok {
		return errEnvelope("session_events", fmt.Errorf("unexpected args type %T", args), "invalid_args")
	}
	sessionID := stringArg(m, "session_id")
	if sessionID == "" {
		return errEnvelope("session_events", fmt.Errorf("session_id required"), "invalid_args")
	}
	toolName := stringArg(m, "tool_name")
	author := stringArg(m, "author")
	eventType := stringArg(m, "event_type")
	limit := 50
	if v, ok := m["limit"].(float64); ok && v > 0 {
		limit = int(v)
	} else if v, ok := m["limit"].(int); ok && v > 0 {
		limit = v
	}
	if limit > 500 {
		limit = 500
	}
	order := stringArg(m, "order")
	if order != "desc" {
		order = "asc"
	}

	stdCtx := toolCtxAsContext(ctx)

	// Authorization: coord can read any session in own root tree
	// (one-level walk for foundation); sub-agent can read only own
	// session.
	rec, err := t.s.sessR.GetSession(stdCtx, ctx.SessionID())
	if err == nil && rec != nil && rec.SessionType != "root" {
		// Sub-agent — only own session allowed.
		if sessionID != ctx.SessionID() {
			return errEnvelope("session_events", ErrSessionNotInScope, "session_not_in_scope")
		}
	}

	rows, err := t.s.runEventsQuery(stdCtx, sessionID, toolName, author, eventType, limit, order)
	if err != nil {
		return errEnvelope("session_events", err, "internal_error")
	}
	out := make([]map[string]any, 0, len(rows))
	for _, h := range rows {
		out = append(out, hitToMap(h))
	}
	return map[string]any{
		"ok":         true,
		"events":     out,
		"count":      len(out),
		"session_id": sessionID,
	}, nil
}

// ─────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────

func stringArg(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func errEnvelope(toolName string, err error, code string) (map[string]any, error) {
	return map[string]any{
		"ok":    false,
		"tool":  toolName,
		"error": err.Error(),
		"code":  code,
	}, nil
}

func toolCtxAsContext(toolCtx tool.Context) context.Context {
	if c, ok := any(toolCtx).(context.Context); ok {
		return c
	}
	return context.Background()
}

func packResult(hits []SearchHit, strategy Strategy, scope Scope, halfLife time.Duration, totalCorpus int) map[string]any {
	out := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		out = append(out, hitToMap(h))
	}
	return map[string]any{
		"ok":                true,
		"hits":              out,
		"strategy":          string(strategy),
		"scope":             string(scope),
		"half_life":         halfLife.String(),
		"total_corpus_size": totalCorpus,
		"count":             len(hits),
	}
}

func hitToMap(h SearchHit) map[string]any {
	out := map[string]any{
		"id":              h.ID,
		"session_id":      h.SessionID,
		"seq":             h.Seq,
		"created_at":      h.CreatedAt.UTC().Format(time.RFC3339),
		"event_type":      h.EventType,
		"author":          h.Author,
		"content_excerpt": h.ContentExcerpt,
	}
	if h.ToolName != "" {
		out["tool_name"] = h.ToolName
	}
	if h.SessionKind != "" {
		out["session_kind"] = h.SessionKind
	}
	if h.Distance != nil {
		out["distance"] = *h.Distance
	}
	if h.Combined > 0 {
		out["combined"] = h.Combined
	}
	if h.RecencyBoost > 0 {
		out["recency_boost"] = h.RecencyBoost
	}
	return out
}

// ─────────────────────────────────────────────────────────────────
// GraphQL query bodies (called from tool Run)
// ─────────────────────────────────────────────────────────────────

type eventRow struct {
	ID         string    `json:"id"`
	SessionID  string    `json:"session_id"`
	Seq        int64     `json:"seq"`
	EventType  string    `json:"event_type"`
	Author     string    `json:"author"`
	ToolName   string    `json:"tool_name"`
	Content    string    `json:"content"`
	CreatedAt  time.Time `json:"created_at"`
	Distance   *float64  `json:"distance,omitempty"`
}

func (s *Service) runTailQuery(ctx context.Context, sessionID string, limit int, dateFrom, dateTo string) ([]SearchHit, error) {
	filter := map[string]any{"session_id": map[string]any{"eq": sessionID}}
	addDateFilter(filter, dateFrom, dateTo)

	rows, err := queries.RunQuery[[]eventRow](ctx, s.q,
		`query ($filter: hub_db_session_events_filter, $limit: Int!) {
			hub { db { agent {
				session_events(
					filter: $filter
					order_by: [{field: "seq", direction: DESC}]
					limit: $limit
				) {
					id session_id seq event_type author tool_name content created_at
				}
			}}}
		}`,
		map[string]any{"filter": filter, "limit": limit},
		"hub.db.agent.session_events",
	)
	if err != nil {
		if errors.Is(err, types.ErrNoData) || errors.Is(err, types.ErrWrongDataPath) {
			return nil, nil
		}
		return nil, fmt.Errorf("search: tail query: %w", err)
	}
	return rowsToHits(rows), nil
}

func (s *Service) runSemantic(ctx context.Context, sessionIDs []string, query string, limit int, dateFrom, dateTo string) ([]SearchHit, error) {
	filter := map[string]any{
		"session_id": map[string]any{"in": sessionIDs},
		"embedding":  map[string]any{"is_null": false},
	}
	addDateFilter(filter, dateFrom, dateTo)

	rows, err := queries.RunQuery[[]eventRow](ctx, s.q,
		`query ($filter: hub_db_session_events_filter, $query: String!, $limit: Int!) {
			hub { db { agent {
				session_events(
					filter: $filter
					semantic: { query: $query, limit: $limit }
				) {
					id session_id seq event_type author tool_name content created_at
					distance: _distance_to_query(query: $query)
				}
			}}}
		}`,
		map[string]any{"filter": filter, "query": query, "limit": limit * 3}, // 3x headroom for rerank
		"hub.db.agent.session_events",
	)
	if err != nil {
		if errors.Is(err, types.ErrNoData) || errors.Is(err, types.ErrWrongDataPath) {
			return nil, nil
		}
		return nil, fmt.Errorf("search: semantic query: %w", err)
	}
	return rowsToHits(rows), nil
}

func (s *Service) runKeyword(ctx context.Context, sessionIDs []string, query string, limit int, dateFrom, dateTo string) ([]SearchHit, error) {
	filter := map[string]any{
		"session_id": map[string]any{"in": sessionIDs},
		"content":    map[string]any{"ilike": "%" + query + "%"},
	}
	addDateFilter(filter, dateFrom, dateTo)

	rows, err := queries.RunQuery[[]eventRow](ctx, s.q,
		`query ($filter: hub_db_session_events_filter, $limit: Int!) {
			hub { db { agent {
				session_events(
					filter: $filter
					order_by: [{field: "created_at", direction: DESC}]
					limit: $limit
				) {
					id session_id seq event_type author tool_name content created_at
				}
			}}}
		}`,
		map[string]any{"filter": filter, "limit": limit * 3},
		"hub.db.agent.session_events",
	)
	if err != nil {
		if errors.Is(err, types.ErrNoData) || errors.Is(err, types.ErrWrongDataPath) {
			return nil, nil
		}
		return nil, fmt.Errorf("search: keyword query: %w", err)
	}
	return rowsToHits(rows), nil
}

func (s *Service) runRecencyOnly(ctx context.Context, sessionIDs []string, limit int, dateFrom, dateTo string) ([]SearchHit, error) {
	filter := map[string]any{
		"session_id": map[string]any{"in": sessionIDs},
	}
	addDateFilter(filter, dateFrom, dateTo)

	rows, err := queries.RunQuery[[]eventRow](ctx, s.q,
		`query ($filter: hub_db_session_events_filter, $limit: Int!) {
			hub { db { agent {
				session_events(
					filter: $filter
					order_by: [{field: "created_at", direction: DESC}]
					limit: $limit
				) {
					id session_id seq event_type author tool_name content created_at
				}
			}}}
		}`,
		map[string]any{"filter": filter, "limit": limit},
		"hub.db.agent.session_events",
	)
	if err != nil {
		if errors.Is(err, types.ErrNoData) || errors.Is(err, types.ErrWrongDataPath) {
			return nil, nil
		}
		return nil, fmt.Errorf("search: recency query: %w", err)
	}
	return rowsToHits(rows), nil
}

func (s *Service) runEventsQuery(ctx context.Context, sessionID, toolName, author, eventType string, limit int, order string) ([]SearchHit, error) {
	filter := map[string]any{"session_id": map[string]any{"eq": sessionID}}
	if toolName != "" {
		filter["tool_name"] = map[string]any{"eq": toolName}
	}
	if author != "" {
		filter["author"] = map[string]any{"eq": author}
	}
	if eventType != "" {
		filter["event_type"] = map[string]any{"eq": eventType}
	}
	dir := "ASC"
	if order == "desc" {
		dir = "DESC"
	}
	q := fmt.Sprintf(`query ($filter: hub_db_session_events_filter, $limit: Int!) {
		hub { db { agent {
			session_events(
				filter: $filter
				order_by: [{field: "seq", direction: %s}]
				limit: $limit
			) {
				id session_id seq event_type author tool_name content created_at
			}
		}}}
	}`, dir)
	rows, err := queries.RunQuery[[]eventRow](ctx, s.q, q,
		map[string]any{"filter": filter, "limit": limit},
		"hub.db.agent.session_events",
	)
	if err != nil {
		if errors.Is(err, types.ErrNoData) || errors.Is(err, types.ErrWrongDataPath) {
			return nil, nil
		}
		return nil, fmt.Errorf("search: events query: %w", err)
	}
	return rowsToHits(rows), nil
}

// addDateFilter appends a created_at clamp to filter when from/to
// are non-empty.
func addDateFilter(filter map[string]any, dateFrom, dateTo string) {
	if dateFrom == "" && dateTo == "" {
		return
	}
	clamp := map[string]any{}
	if dateFrom != "" {
		clamp["gte"] = dateFrom
	}
	if dateTo != "" {
		clamp["lte"] = dateTo
	}
	filter["created_at"] = clamp
}

func rowsToHits(rows []eventRow) []SearchHit {
	out := make([]SearchHit, 0, len(rows))
	for _, r := range rows {
		out = append(out, SearchHit{
			ID:             r.ID,
			SessionID:      r.SessionID,
			Seq:            r.Seq,
			CreatedAt:      r.CreatedAt,
			EventType:      r.EventType,
			ToolName:       r.ToolName,
			Author:         r.Author,
			ContentExcerpt: truncateRunes(r.Content, 400),
			Distance:       r.Distance,
		})
	}
	return out
}

func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
