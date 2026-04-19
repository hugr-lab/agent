package session

import (
	"iter"
	"slices"
	"sync"

	adksession "google.golang.org/adk/session"
)

// eventStore is a goroutine-safe backing store for adksession.Events.
type eventStore struct {
	mu     sync.RWMutex
	events []*adksession.Event
}

var _ adksession.Events = (*eventStore)(nil)

func newEventStore() *eventStore {
	return &eventStore{}
}

func (e *eventStore) append(ev *adksession.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, ev)
}

func (e *eventStore) snapshot() []*adksession.Event {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return slices.Clone(e.events)
}

// All returns an iterator over a point-in-time snapshot of the events.
func (e *eventStore) All() iter.Seq[*adksession.Event] {
	evs := e.snapshot()
	return func(yield func(*adksession.Event) bool) {
		for _, ev := range evs {
			if !yield(ev) {
				return
			}
		}
	}
}

func (e *eventStore) Len() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.events)
}

func (e *eventStore) At(i int) *adksession.Event {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if i < 0 || i >= len(e.events) {
		return nil
	}
	return e.events[i]
}
