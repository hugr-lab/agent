package memory

import (
	"fmt"
	"sync"
	"time"

	memdb "github.com/hugr-lab/hugen/pkg/store/memory"
	sessdb "github.com/hugr-lab/hugen/pkg/store/sessions"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
)

// injectorTTL is the cache lifetime for a rendered memory-status
// block per session. Short enough that the hint reflects recent
// activity, long enough that we don't re-query hub on every turn.
const injectorTTL = 10 * time.Second

// InstructionProvider is a locally-scoped alias so the callers
// (cmd/agent/runtime.go) don't need a direct llmagent import when
// composing instruction providers.
type InstructionProvider = llmagent.InstructionProvider

// WrapInstruction returns a new InstructionProvider that appends a
// runtime-computed "## Memory Status" block to the base provider's
// output. The block summarises long-term memory + session notes for
// the current session, cached per-session for 10s.
//
// This is the ADK-native hook for runtime-computed system
// instructions (spec 005 research Decision 4). `Session.Snapshot`
// continues to return the state-only base prompt — the hint sits on
// top.
func WrapInstruction(base InstructionProvider, memory *memdb.Client, sessions *sessdb.Client) InstructionProvider {
	if memory == nil {
		return base
	}
	cache := &injectorCache{entries: map[string]injectorEntry{}}
	return func(ctx agent.ReadonlyContext) (string, error) {
		basePrompt, err := base(ctx)
		if err != nil {
			return "", err
		}
		sid := ctx.SessionID()
		if sid == "" {
			return basePrompt, nil
		}
		hint := cache.get(sid)
		if hint == "" {
			hint = renderStatus(ctx, sid, memory, sessions)
			cache.put(sid, hint)
		}
		if hint == "" {
			return basePrompt, nil
		}
		return basePrompt + "\n\n" + hint, nil
	}
}

// renderStatus builds a single "## Memory Status" line from
// memory.Hint + sessions.ListNotes count.
func renderStatus(ctx agent.ReadonlyContext, sid string, memory *memdb.Client, sessions *sessdb.Client) string {
	h, err := memory.Hint(ctx, "", nil)
	if err != nil || h == "" {
		return ""
	}
	out := "## Memory Status\n" + h
	if sessions != nil {
		if notes, err := sessions.ListNotes(ctx, sid); err == nil && len(notes) > 0 {
			out += fmt.Sprintf(". Session notes: %d.", len(notes))
		}
	}
	out += "\nUse memory_search(query, tags?) to retrieve. Use memory_note(content) to save."
	return out
}

// injectorCache is a tiny per-process TTL cache keyed by session id.
// We keep it here rather than pulling in an LRU dep (constitution §V).
type injectorCache struct {
	mu      sync.Mutex
	entries map[string]injectorEntry
}

type injectorEntry struct {
	expires time.Time
	text    string
}

func (c *injectorCache) get(sid string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[sid]
	if !ok || time.Now().After(e.expires) {
		return ""
	}
	return e.text
}

func (c *injectorCache) put(sid, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[sid] = injectorEntry{
		expires: time.Now().Add(injectorTTL),
		text:    text,
	}
}
