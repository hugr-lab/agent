package planner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/hugr-lab/hugen/pkg/missions/graph"
)

// planCache is a per-session LRU with TTL + single-flight sentinel.
// Keyed by (coordSessionID, sha256(goal + skillsDigest)); collapses
// concurrent same-key calls to one LLM call.
type planCache struct {
	ttl time.Duration
	cap int

	mu       sync.Mutex
	sessions map[string]*sessionRing
}

func newPlanCache(ttl time.Duration, cap_ int) *planCache {
	return &planCache{
		ttl:      ttl,
		cap:      cap_,
		sessions: make(map[string]*sessionRing),
	}
}

// sessionRing is one coordinator session's cache slice. Access order
// is maintained in order (newest last); eviction truncates the head.
type sessionRing struct {
	entries  []ringEntry
	inflight map[string]*inflightEntry
}

type ringEntry struct {
	key       string
	plan      graph.PlanResult
	expiresAt time.Time
}

type inflightEntry struct {
	done chan struct{}
	plan graph.PlanResult
	ok   bool
}

func (c *planCache) get(sessionID, key string) (graph.PlanResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ring, ok := c.sessions[sessionID]
	if !ok {
		return graph.PlanResult{}, false
	}
	now := time.Now()
	for i := len(ring.entries) - 1; i >= 0; i-- {
		e := ring.entries[i]
		if e.key != key {
			continue
		}
		if now.After(e.expiresAt) {
			ring.entries = append(ring.entries[:i], ring.entries[i+1:]...)
			return graph.PlanResult{}, false
		}
		return e.plan, true
	}
	return graph.PlanResult{}, false
}

// acquire returns (true, nil) if the caller is the leader and should
// proceed to call the LLM. Returns (false, wait) when another call
// is already in flight for the same key; the caller invokes wait to
// block until the leader publishes (or fails).
func (c *planCache) acquire(sessionID, key string) (bool, func(context.Context) (graph.PlanResult, bool)) {
	c.mu.Lock()
	ring := c.ensureRing(sessionID)
	if entry, ok := ring.inflight[key]; ok {
		c.mu.Unlock()
		return false, func(ctx context.Context) (graph.PlanResult, bool) {
			select {
			case <-entry.done:
				if entry.ok {
					return entry.plan, true
				}
				return graph.PlanResult{}, false
			case <-ctx.Done():
				return graph.PlanResult{}, false
			}
		}
	}
	if ring.inflight == nil {
		ring.inflight = map[string]*inflightEntry{}
	}
	ring.inflight[key] = &inflightEntry{done: make(chan struct{})}
	c.mu.Unlock()
	return true, nil
}

// release signals completion of the leader's work. Must be paired
// with acquire=true.
func (c *planCache) release(sessionID, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ring := c.sessions[sessionID]
	if ring == nil {
		return
	}
	entry := ring.inflight[key]
	if entry == nil {
		return
	}
	select {
	case <-entry.done:
	default:
		close(entry.done)
	}
	delete(ring.inflight, key)
}

func (c *planCache) put(sessionID, key string, plan graph.PlanResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ring := c.ensureRing(sessionID)
	if entry := ring.inflight[key]; entry != nil {
		entry.plan = plan
		entry.ok = true
	}
	for i := 0; i < len(ring.entries); i++ {
		if ring.entries[i].key == key {
			ring.entries = append(ring.entries[:i], ring.entries[i+1:]...)
			break
		}
	}
	ring.entries = append(ring.entries, ringEntry{
		key:       key,
		plan:      plan,
		expiresAt: time.Now().Add(c.ttl),
	})
	if len(ring.entries) > c.cap {
		ring.entries = ring.entries[len(ring.entries)-c.cap:]
	}
}

// markFailure signals the leader's work failed; waiters wake and see
// ok=false, prompting them to retry from scratch.
func (c *planCache) markFailure(sessionID, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ring := c.sessions[sessionID]
	if ring == nil {
		return
	}
	if entry := ring.inflight[key]; entry != nil {
		entry.ok = false
	}
}

func (c *planCache) dropSession(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ring, ok := c.sessions[sessionID]; ok {
		for _, entry := range ring.inflight {
			entry.ok = false
			select {
			case <-entry.done:
			default:
				close(entry.done)
			}
		}
	}
	delete(c.sessions, sessionID)
}

func (c *planCache) ensureRing(sessionID string) *sessionRing {
	ring, ok := c.sessions[sessionID]
	if !ok {
		ring = &sessionRing{}
		c.sessions[sessionID] = ring
	}
	return ring
}

func cacheKey(coordSessionID, goal, skillsDigest string) string {
	return coordSessionID + "\x00" + hashSHA256(goal+"\x00"+skillsDigest)
}

func hashSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
