// Memory GraphQL bindings for HubDB.Memory and the
// memory_tags / memory_links / memory_log satellite tables.
//
// APPEND-ONLY: Reinforce/Supersede = DELETE + INSERT, never UPDATE.
// Every state change emits one or more memory_log rows. When a single
// HubDB call writes multiple log rows (e.g. Reinforce bundles
// reinforce + add_tag×N + add_link×M) the adapter uses a single
// baseTime and offsets each row by idx*1µs so the composite PK
// (event_time, event_type, memory_item_id, session_id) never
// collides within the batch.
package memory

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/pkg/store/internal/qh"

	"github.com/hugr-lab/hugen/pkg/id"
)

// baseTimeForBatch returns the baseline timestamp a batch of memory_log
// rows should share. All rows in the batch then call offsetMicro with
// successive idx values so their composite primary key
// (event_time, event_type, memory_item_id, session_id) stays collision-
// free even when the wall clock resolves only to microseconds.
func baseTimeForBatch() time.Time { return time.Now().UTC() }

// offsetMicro returns base + idx*1µs. Caller passes idx=0 for the first
// row and increments monotonically within the batch.
func offsetMicro(base time.Time, idx int) time.Time {
	return base.Add(time.Duration(idx) * time.Microsecond)
}


// ------------------------------------------------------------
// Read path
// ------------------------------------------------------------

type memoryRow struct {
	ID         string  `json:"id"`
	AgentID    string  `json:"agent_id"`
	Content    string  `json:"content"`
	Category   string  `json:"category"`
	Volatility string  `json:"volatility"`
	Score      float64 `json:"score"`
	Source     string  `json:"source"`
	ValidFrom  qh.DBTime  `json:"valid_from"`
	ValidTo    qh.DBTime  `json:"valid_to"`
	CreatedAt  qh.DBTime  `json:"created_at"`
	IsValid    bool    `json:"is_valid"`
	AgeDays    int     `json:"age_days"`
	ExpiresIn  int     `json:"expires_in_days"`
	Distance   float64 `json:"_embedding_distance"`
}

type memoryTagRow struct {
	Tag string `json:"tag"`
}

type memoryLinkRow struct {
	TargetID string `json:"target_id"`
	Relation string `json:"relation"`
}

func toSearchResult(r memoryRow, tags []memoryTagRow, links []memoryLinkRow) SearchResult {
	res := SearchResult{
		Item: Item{
			ID:         r.ID,
			AgentID:    r.AgentID,
			Content:    r.Content,
			Category:   r.Category,
			Volatility: r.Volatility,
			Score:      r.Score,
			Source:     r.Source,
			ValidFrom:  r.ValidFrom.Time,
			ValidTo:    r.ValidTo.Time,
			CreatedAt:  r.CreatedAt.Time,
		},
		IsValid:       r.IsValid,
		AgeDays:       r.AgeDays,
		ExpiresInDays: r.ExpiresIn,
		Distance:      r.Distance,
	}
	for _, t := range tags {
		res.Tags = append(res.Tags, t.Tag)
	}
	for _, l := range links {
		res.Links = append(res.Links, l.TargetID)
	}
	return res
}

// Search returns memory items matching the query. Applies the agent
// scope + valid_to > now by default. When embedding is provided and the
// engine exposes similarity, results are ordered by cosine distance;
// otherwise the query-string is used as an ILIKE pattern.
func (c *Client) Search(ctx context.Context, query string, embedding []float32, opts SearchOpts) ([]SearchResult, error) {
	type row struct {
		memoryRow
		Tags  []memoryTagRow  `json:"tags"`
		Links []memoryLinkRow `json:"outgoing_links"`
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 5
	}
	filter := map[string]any{"agent_id": map[string]any{"eq": c.agentID}}
	if opts.ValidOnly || opts.ValidAt == nil {
		nowCutoff := time.Now().UTC().Format(time.RFC3339)
		if opts.ValidAt != nil {
			nowCutoff = opts.ValidAt.UTC().Format(time.RFC3339)
		}
		filter["valid_to"] = map[string]any{"gt": nowCutoff}
	}
	if opts.Category != "" {
		filter["category"] = map[string]any{"eq": opts.Category}
	}
	if opts.MinScore > 0 {
		filter["score"] = map[string]any{"gte": opts.MinScore}
	}
	vars := map[string]any{
		"filter": filter,
		"limit":  limit,
	}
	// Keyword/ILIKE fallback when we have no embedding or embeddings
	// are not enabled. Applies to `content` against the raw query.
	q := `query ($filter: hub_db_memory_items_filter, $limit: Int!) {
			hub { db { agent {
				memory_items(filter: $filter, limit: $limit, order_by: [{field: "score", direction: DESC}]) {
					id agent_id content category volatility score source
					valid_from valid_to created_at
					is_valid age_days expires_in_days
					tags { tag }
					outgoing_links { target_id relation }
				}
			}}}
		}`
	if len(embedding) > 0 {
		vars["vec"] = embedding
		q = `query ($filter: hub_db_memory_items_filter, $limit: Int!, $vec: [Float!]!) {
				hub { db { agent {
					memory_items(
						filter: $filter
						limit: $limit
						similarity: {name: "embedding", vector: $vec, distance: Cosine, limit: $limit}
						order_by: [{field: "_embedding_distance", direction: ASC}]
					) {
						id agent_id content category volatility score source
						valid_from valid_to created_at
						is_valid age_days expires_in_days
						_embedding_distance(vector: $vec, distance: Cosine)
						tags { tag }
						outgoing_links { target_id relation }
					}
				}}}
			}`
	} else if query != "" {
		// Naive ILIKE fallback; a richer FTS setup can replace this later.
		filter["content"] = map[string]any{"ilike": "%" + query + "%"}
	}
	rows, err := qh.RunQuery[[]row](ctx, c.querier, q, vars, "hub.db.agent.memory_items")
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}

	results := make([]SearchResult, 0, len(rows))
	// Filter by tags client-side if requested — avoids a more complex
	// nested filter against the GraphQL compiler.
	for _, r := range rows {
		if len(opts.Tags) > 0 {
			matched := 0
			have := map[string]bool{}
			for _, t := range r.Tags {
				have[t.Tag] = true
			}
			for _, want := range opts.Tags {
				if have[want] {
					matched++
				}
			}
			if matched != len(opts.Tags) {
				continue
			}
		}
		results = append(results, toSearchResult(r.memoryRow, r.Tags, r.Links))
	}

	// Retrieve log entries for every hit (used by used_count / last_used
	// aggregates). Best-effort: failures here are not fatal to the read.
	if len(results) > 0 {
		_ = c.logRetrievals(ctx, results, sessionIDFrom(ctx))
	}
	return results, nil
}

// Get fetches a single memory item by id. Returns (nil, nil) when
// the item does not exist.
func (c *Client) Get(ctx context.Context, memID string) (*SearchResult, error) {
	type row struct {
		memoryRow
		Tags  []memoryTagRow  `json:"tags"`
		Links []memoryLinkRow `json:"outgoing_links"`
	}
	rows, err := qh.RunQuery[[]row](ctx, c.querier,
		`query ($agent: String!, $id: String!) {
			hub { db { agent {
				memory_items(filter: {agent_id: {eq: $agent}, id: {eq: $id}}, limit: 1) {
					id agent_id content category volatility score source
					valid_from valid_to created_at
					is_valid age_days expires_in_days
					tags { tag }
					outgoing_links { target_id relation }
				}
			}}}
		}`,
		map[string]any{"agent": c.agentID, "id": memID},
		"hub.db.agent.memory_items",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	res := toSearchResult(rows[0].memoryRow, rows[0].Tags, rows[0].Links)
	return &res, nil
}

// GetLinked returns facts reachable from memID through outgoing links
// up to the requested depth. Performed client-side as a BFS because
// the schema doesn't provide a recursive view for memory_links.
func (c *Client) GetLinked(ctx context.Context, memID string, depth int) ([]SearchResult, error) {
	if depth <= 0 {
		depth = 1
	}
	visited := map[string]bool{memID: true}
	frontier := []string{memID}
	var collected []SearchResult
	for level := 0; level < depth && len(frontier) > 0; level++ {
		next := frontier
		frontier = nil
		for _, src := range next {
			row, err := c.Get(ctx, src)
			if err != nil {
				return nil, err
			}
			if row == nil {
				continue
			}
			if level > 0 { // seed itself is not in the result set
				collected = append(collected, *row)
			}
			for _, targetID := range row.Links {
				if visited[targetID] {
					continue
				}
				visited[targetID] = true
				frontier = append(frontier, targetID)
			}
		}
	}
	return collected, nil
}

// Stats returns counts of memory items grouped by category + aggregate
// figures. Implemented as a small set of targeted queries rather than
// a GraphQL aggregate — keeps us portable across compiler versions.
func (c *Client) Stats(ctx context.Context) (Stats, error) {
	type row struct {
		ID        string  `json:"id"`
		Category  string  `json:"category"`
		ValidTo   qh.DBTime  `json:"valid_to"`
		ValidFrom qh.DBTime  `json:"valid_from"`
		IsValid   bool    `json:"is_valid"`
		_         float64 `json:"-"`
	}
	rows, err := qh.RunQuery[[]row](ctx, c.querier,
		`query ($agent: String!) {
			hub { db { agent {
				memory_items(filter: {agent_id: {eq: $agent}}) {
					id category valid_to valid_from is_valid
				}
			}}}
		}`,
		map[string]any{"agent": c.agentID},
		"hub.db.agent.memory_items",
	)
	if err != nil && !errors.Is(err, types.ErrWrongDataPath) && !errors.Is(err, types.ErrNoData) {
		return Stats{}, err
	}
	stats := Stats{ByCategory: map[string]int{}}
	for _, r := range rows {
		stats.TotalItems++
		if r.IsValid {
			stats.ActiveItems++
		}
		stats.ByCategory[r.Category]++
		if !r.ValidFrom.Time.IsZero() {
			if stats.OldestFact.IsZero() || r.ValidFrom.Time.Before(stats.OldestFact) {
				stats.OldestFact = r.ValidFrom.Time
			}
			if stats.NewestFact.IsZero() || r.ValidFrom.Time.After(stats.NewestFact) {
				stats.NewestFact = r.ValidFrom.Time
			}
		}
	}

	// Pending reviews — cheap single count.
	type rev struct {
		ID string `json:"id"`
	}
	revs, err := qh.RunQuery[[]rev](ctx, c.querier,
		`query ($agent: String!) {
			hub { db { agent {
				session_reviews(filter: {agent_id: {eq: $agent}, status: {eq: "pending"}}) { id }
			}}}
		}`,
		map[string]any{"agent": c.agentID},
		"hub.db.agent.session_reviews",
	)
	if err == nil {
		stats.PendingReview = len(revs)
	}
	return stats, nil
}

// Hint renders the short "Memory Status" line shown at the top of the
// system prompt every turn. query / embedding are unused in the
// initial implementation — we always render the aggregate status.
// Kept on the signature so a future richer hint (e.g. top matches
// for the current query) can be added without breaking callers.
func (c *Client) Hint(ctx context.Context, query string, embedding []float32) (string, error) {
	_ = query
	_ = embedding
	stats, err := c.Stats(ctx)
	if err != nil {
		return "", err
	}
	s := fmt.Sprintf("%d long-term facts", stats.ActiveItems)
	if len(stats.ByCategory) > 0 {
		s += " ("
		i := 0
		for cat, n := range stats.ByCategory {
			if i > 0 {
				s += ", "
			}
			s += fmt.Sprintf("%d %s", n, cat)
			i++
		}
		s += ")"
	}
	if !stats.OldestFact.IsZero() {
		age := int(time.Since(stats.OldestFact).Hours() / 24)
		s += fmt.Sprintf(". Oldest: %d days ago", age)
	}
	if stats.PendingReview > 0 {
		s += fmt.Sprintf(". Pending reviews: %d", stats.PendingReview)
	}
	return s, nil
}

// ------------------------------------------------------------
// Write path
// ------------------------------------------------------------

// Store inserts a new memory_item plus its tags and links, then a
// matching memory_log entry. Returns the assigned ID (caller may pre-
// fill item.ID; otherwise the adapter generates one via pkg/id).
func (c *Client) Store(ctx context.Context, item Item, tags []string, links []Link) (string, error) {
	if item.Content == "" {
		return "", fmt.Errorf("hubdb: Store requires Content")
	}
	if item.ID == "" {
		item.ID = id.New(id.PrefixMemory, c.agentShort)
	}
	if item.AgentID == "" {
		item.AgentID = c.agentID
	}
	now := time.Now().UTC()
	if item.ValidFrom.IsZero() {
		item.ValidFrom = now
	}
	if item.ValidTo.IsZero() {
		item.ValidTo = now.AddDate(1, 0, 0) // fallback: 1 year; reviewer normally sets this
	}
	if item.Volatility == "" {
		item.Volatility = "stable"
	}
	data := map[string]any{
		"id":         item.ID,
		"agent_id":   item.AgentID,
		"content":    item.Content,
		"category":   item.Category,
		"volatility": item.Volatility,
		"score":      item.Score,
		"source":     item.Source,
		"valid_from": item.ValidFrom.UTC().Format(time.RFC3339),
		"valid_to":   item.ValidTo.UTC().Format(time.RFC3339),
	}
	if err := qh.RunMutation(ctx, c.querier,
		`mutation ($data: hub_db_memory_items_mut_input_data!) {
			hub { db { agent {
				insert_memory_items(data: $data) { id }
			}}}
		}`,
		map[string]any{"data": data},
	); err != nil {
		return "", err
	}
	if err := c.insertTags(ctx, item.ID, tags); err != nil {
		return item.ID, err
	}
	if err := c.insertLinks(ctx, item.ID, links); err != nil {
		return item.ID, err
	}
	base := baseTimeForBatch()
	sid := sessionIDFrom(ctx)
	logs := []LogEntry{{
		EventTime:    offsetMicro(base, 0),
		EventType:    "store",
		MemoryItemID: item.ID,
		SessionID:    sid,
		AgentID:      item.AgentID,
		Details:      map[string]any{"category": item.Category, "score": item.Score},
	}}
	for i, tag := range tags {
		logs = append(logs, LogEntry{
			EventTime: offsetMicro(base, i+1), EventType: "add_tag",
			MemoryItemID: item.ID, SessionID: sid, AgentID: item.AgentID,
			Details: map[string]any{"tag": tag},
		})
	}
	for i, link := range links {
		logs = append(logs, LogEntry{
			EventTime: offsetMicro(base, 1+len(tags)+i), EventType: "add_link",
			MemoryItemID: item.ID, SessionID: sid, AgentID: item.AgentID,
			Details: map[string]any{"target_id": link.TargetID, "relation": link.Relation},
		})
	}
	if err := c.logBatch(ctx, logs); err != nil {
		return item.ID, err
	}
	return item.ID, nil
}

// Reinforce is an append-only score bump: delete the old row + insert
// a fresh one with the same ID, merged tags/links, bumped score, and
// refreshed valid_to. Volatility duration is not touched here —
// callers handle it by passing a bonus that implicitly represents a
// validity refresh as well.
func (c *Client) Reinforce(ctx context.Context, memID string, scoreBonus float64, extraTags []string, extraLinks []Link) error {
	existing, err := c.Get(ctx, memID)
	if err != nil {
		return err
	}
	if existing == nil {
		return fmt.Errorf("hubdb: Reinforce: memory item %q not found", memID)
	}
	newScore := existing.Score + scoreBonus
	if newScore > 1 {
		newScore = 1
	}
	// Merge tags
	existingTags := map[string]bool{}
	merged := existing.Tags
	for _, t := range existing.Tags {
		existingTags[t] = true
	}
	for _, t := range extraTags {
		if !existingTags[t] {
			merged = append(merged, t)
			existingTags[t] = true
		}
	}
	// Merge links
	existingLinks := map[string]bool{}
	for _, t := range existing.Links {
		existingLinks[t] = true
	}
	mergedLinks := make([]Link, 0, len(extraLinks))
	for _, l := range extraLinks {
		if existingLinks[l.TargetID] {
			continue
		}
		existingLinks[l.TargetID] = true
		mergedLinks = append(mergedLinks, l)
	}
	if err := c.deleteItem(ctx, memID); err != nil {
		return err
	}
	// Re-insert with same ID, bumped score, refreshed valid_to
	item := existing.Item
	item.Score = newScore
	item.ValidFrom = time.Now().UTC()
	// valid_to stays at its current value (reviewer recomputes on
	// volatility-driven refresh); we only extend if it already passed
	// now, in which case treat it as a fresh insert.
	if item.ValidTo.Before(item.ValidFrom) {
		item.ValidTo = item.ValidFrom.AddDate(1, 0, 0)
	}
	data := map[string]any{
		"id":         item.ID,
		"agent_id":   item.AgentID,
		"content":    item.Content,
		"category":   item.Category,
		"volatility": item.Volatility,
		"score":      item.Score,
		"source":     item.Source,
		"valid_from": item.ValidFrom.UTC().Format(time.RFC3339),
		"valid_to":   item.ValidTo.UTC().Format(time.RFC3339),
	}
	if err := qh.RunMutation(ctx, c.querier,
		`mutation ($data: hub_db_memory_items_mut_input_data!) {
			hub { db { agent {
				insert_memory_items(data: $data) { id }
			}}}
		}`,
		map[string]any{"data": data},
	); err != nil {
		return err
	}
	if err := c.insertTags(ctx, memID, merged); err != nil {
		return err
	}
	if err := c.insertLinks(ctx, memID, mergedLinks); err != nil {
		return err
	}
	base := baseTimeForBatch()
	sid := sessionIDFrom(ctx)
	logs := []LogEntry{{
		EventTime: offsetMicro(base, 0), EventType: "reinforce",
		MemoryItemID: memID, SessionID: sid, AgentID: item.AgentID,
		Details: map[string]any{"old_score": existing.Score, "new_score": newScore},
	}}
	for i, tag := range extraTags {
		logs = append(logs, LogEntry{
			EventTime: offsetMicro(base, i+1), EventType: "add_tag",
			MemoryItemID: memID, SessionID: sid, AgentID: item.AgentID,
			Details: map[string]any{"tag": tag},
		})
	}
	for i, l := range mergedLinks {
		logs = append(logs, LogEntry{
			EventTime: offsetMicro(base, 1+len(extraTags)+i), EventType: "add_link",
			MemoryItemID: memID, SessionID: sid, AgentID: item.AgentID,
			Details: map[string]any{"target_id": l.TargetID, "relation": l.Relation},
		})
	}
	return c.logBatch(ctx, logs)
}

// Supersede deletes an old fact and inserts a new one with a link
// back to the deleted predecessor (relation="supersedes").
func (c *Client) Supersede(ctx context.Context, oldID string, newItem Item, tags []string, links []Link) (string, error) {
	existing, err := c.Get(ctx, oldID)
	if err != nil {
		return "", err
	}
	if existing == nil {
		return "", fmt.Errorf("hubdb: Supersede: memory item %q not found", oldID)
	}
	if err := c.deleteItem(ctx, oldID); err != nil {
		return "", err
	}
	// Link new → old to preserve history.
	links = append(links, Link{
		TargetID: oldID,
		Relation: "supersedes",
	})
	newID, err := c.Store(ctx, newItem, tags, links)
	if err != nil {
		return "", err
	}
	// Audit log for the supersede event explicitly.
	return newID, c.Log(ctx, LogEntry{
		EventTime: time.Now().UTC(), EventType: "supersede",
		MemoryItemID: oldID, SessionID: sessionIDFrom(ctx), AgentID: c.agentID,
		Details: map[string]any{"superseded_by": newID},
	})
}

// Delete removes a memory item, its tags, its outgoing links, and
// marks inbound links as expired. Logs a delete audit entry.
func (c *Client) Delete(ctx context.Context, memID string) error {
	if err := c.deleteItem(ctx, memID); err != nil {
		return err
	}
	return c.Log(ctx, LogEntry{
		EventTime: time.Now().UTC(), EventType: "delete",
		MemoryItemID: memID, SessionID: sessionIDFrom(ctx), AgentID: c.agentID,
	})
}

// DeleteExpired removes every memory_item whose valid_to has passed.
// Returns the affected_rows count. Writes a single aggregate memory_log
// entry rather than one per deleted row.
func (c *Client) DeleteExpired(ctx context.Context) (int, error) {
	type result struct {
		Affected int `json:"affected_rows"`
	}
	resp, err := c.querier.Query(ctx,
		`mutation ($agent: String!, $now: Timestamp!) {
			hub { db { agent {
				delete_memory_items(filter: {agent_id: {eq: $agent}, valid_to: {lt: $now}}) {
					affected_rows
				}
			}}}
		}`,
		map[string]any{
			"agent": c.agentID,
			"now":   time.Now().UTC().Format(time.RFC3339),
		},
	)
	if err != nil {
		return 0, fmt.Errorf("hubdb mutation: %w", err)
	}
	defer resp.Close()
	if err := resp.Err(); err != nil {
		return 0, fmt.Errorf("hubdb graphql: %w", err)
	}
	var r result
	if err := resp.ScanData("hub.db.agent.delete_memory_items", &r); err != nil {
		if !errors.Is(err, types.ErrWrongDataPath) && !errors.Is(err, types.ErrNoData) {
			return 0, fmt.Errorf("hubdb scan: %w", err)
		}
	}
	if r.Affected > 0 {
		_ = c.Log(ctx, LogEntry{
			EventTime: time.Now().UTC(), EventType: "delete_expired",
			AgentID: c.agentID,
			Details: map[string]any{"count": r.Affected},
		})
	}
	return r.Affected, nil
}

// AddTags inserts one memory_tags row per tag and a memory_log entry
// per row. Uses µs-offset timestamps.
func (c *Client) AddTags(ctx context.Context, memID string, tags []string) error {
	if err := c.insertTags(ctx, memID, tags); err != nil {
		return err
	}
	base := baseTimeForBatch()
	sid := sessionIDFrom(ctx)
	logs := make([]LogEntry, 0, len(tags))
	for i, t := range tags {
		logs = append(logs, LogEntry{
			EventTime: offsetMicro(base, i), EventType: "add_tag",
			MemoryItemID: memID, SessionID: sid, AgentID: c.agentID,
			Details: map[string]any{"tag": t},
		})
	}
	return c.logBatch(ctx, logs)
}

// RemoveTags deletes memory_tags rows and logs the operation.
func (c *Client) RemoveTags(ctx context.Context, memID string, tags []string) error {
	for _, t := range tags {
		if err := qh.RunMutation(ctx, c.querier,
			`mutation ($mid: String!, $tag: String!) {
				hub { db { agent {
					delete_memory_tags(filter: {memory_item_id: {eq: $mid}, tag: {eq: $tag}}) {
						affected_rows
					}
				}}}
			}`,
			map[string]any{"mid": memID, "tag": t},
		); err != nil {
			return err
		}
	}
	base := baseTimeForBatch()
	sid := sessionIDFrom(ctx)
	logs := make([]LogEntry, 0, len(tags))
	for i, t := range tags {
		logs = append(logs, LogEntry{
			EventTime: offsetMicro(base, i), EventType: "remove_tag",
			MemoryItemID: memID, SessionID: sid, AgentID: c.agentID,
			Details: map[string]any{"tag": t},
		})
	}
	return c.logBatch(ctx, logs)
}

// AddLink inserts a single memory_links row + audit entry.
func (c *Client) AddLink(ctx context.Context, link Link) error {
	data := map[string]any{
		"source_id": link.SourceID,
		"target_id": link.TargetID,
		"relation":  link.Relation,
	}
	if err := qh.RunMutation(ctx, c.querier,
		`mutation ($data: hub_db_memory_links_mut_input_data!) {
			hub { db { agent {
				insert_memory_links(data: $data) { source_id target_id }
			}}}
		}`,
		map[string]any{"data": data},
	); err != nil {
		return err
	}
	return c.Log(ctx, LogEntry{
		EventTime: time.Now().UTC(), EventType: "add_link",
		MemoryItemID: link.SourceID, SessionID: sessionIDFrom(ctx), AgentID: c.agentID,
		Details: map[string]any{"target_id": link.TargetID, "relation": link.Relation},
	})
}

// RemoveLink deletes a memory_links row + audit entry.
func (c *Client) RemoveLink(ctx context.Context, sourceID, targetID string) error {
	if err := qh.RunMutation(ctx, c.querier,
		`mutation ($src: String!, $tgt: String!) {
			hub { db { agent {
				delete_memory_links(filter: {source_id: {eq: $src}, target_id: {eq: $tgt}}) { affected_rows }
			}}}
		}`,
		map[string]any{"src": sourceID, "tgt": targetID},
	); err != nil {
		return err
	}
	return c.Log(ctx, LogEntry{
		EventTime: time.Now().UTC(), EventType: "remove_link",
		MemoryItemID: sourceID, SessionID: sessionIDFrom(ctx), AgentID: c.agentID,
		Details: map[string]any{"target_id": targetID},
	})
}

// ------------------------------------------------------------
// internal helpers
// ------------------------------------------------------------

func (c *Client) insertTags(ctx context.Context, memID string, tags []string) error {
	for _, t := range tags {
		if t == "" {
			continue
		}
		if err := qh.RunMutation(ctx, c.querier,
			`mutation ($data: hub_db_memory_tags_mut_input_data!) {
				hub { db { agent {
					insert_memory_tags(data: $data) { memory_item_id tag }
				}}}
			}`,
			map[string]any{"data": map[string]any{"memory_item_id": memID, "tag": t}},
		); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) insertLinks(ctx context.Context, sourceID string, links []Link) error {
	for _, l := range links {
		if l.TargetID == "" {
			continue
		}
		if err := qh.RunMutation(ctx, c.querier,
			`mutation ($data: hub_db_memory_links_mut_input_data!) {
				hub { db { agent {
					insert_memory_links(data: $data) { source_id target_id }
				}}}
			}`,
			map[string]any{"data": map[string]any{
				"source_id": sourceID,
				"target_id": l.TargetID,
				"relation":  l.Relation,
			}},
		); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) deleteItem(ctx context.Context, memID string) error {
	if err := qh.RunMutation(ctx, c.querier,
		`mutation ($mid: String!) {
			hub { db { agent {
				delete_memory_tags(filter: {memory_item_id: {eq: $mid}}) { affected_rows }
			}}}
		}`,
		map[string]any{"mid": memID},
	); err != nil {
		return err
	}
	if err := qh.RunMutation(ctx, c.querier,
		`mutation ($mid: String!) {
			hub { db { agent {
				delete_memory_links(filter: {source_id: {eq: $mid}}) { affected_rows }
			}}}
		}`,
		map[string]any{"mid": memID},
	); err != nil {
		return err
	}
	return qh.RunMutation(ctx, c.querier,
		`mutation ($mid: String!) {
			hub { db { agent {
				delete_memory_items(filter: {id: {eq: $mid}}) { affected_rows }
			}}}
		}`,
		map[string]any{"mid": memID},
	)
}

func (c *Client) logRetrievals(ctx context.Context, results []SearchResult, sessionID string) error {
	base := baseTimeForBatch()
	logs := make([]LogEntry, 0, len(results))
	for i, r := range results {
		logs = append(logs, LogEntry{
			EventTime:    offsetMicro(base, i),
			EventType:    "retrieve",
			MemoryItemID: r.ID,
			SessionID:    sessionID,
			AgentID:      c.agentID,
		})
	}
	return c.logBatch(ctx, logs)
}
