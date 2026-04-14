// Package agent implements the custom HugrAgent loop with dynamic tool management.
package agent

import (
	"sync"
	"time"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/tool"
)

// DynamicToolset is a thread-safe tool.Toolset that allows adding and removing
// child toolsets at runtime. It supports both global children (visible to all
// sessions) and session-scoped children (visible only to a specific session).
//
// Tools() merges global + session-specific children on every call,
// so the LLM sees an up-to-date tool list each turn.
type DynamicToolset struct {
	mu              sync.RWMutex
	children        map[string]tool.Toolset            // global (system tools)
	sessionChildren map[string]map[string]tool.Toolset // sessionID -> name -> toolset
	sessionAccess   map[string]time.Time               // sessionID -> last access time
}

var _ tool.Toolset = (*DynamicToolset)(nil)

// NewDynamicToolset creates an empty DynamicToolset.
func NewDynamicToolset() *DynamicToolset {
	return &DynamicToolset{
		children:        make(map[string]tool.Toolset),
		sessionChildren: make(map[string]map[string]tool.Toolset),
		sessionAccess:   make(map[string]time.Time),
	}
}

func (d *DynamicToolset) Name() string { return "dynamic" }

// Tools merges tools from global and session-specific child toolsets.
func (d *DynamicToolset) Tools(ctx agent.ReadonlyContext) ([]tool.Tool, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var all []tool.Tool

	// Global tools (always visible).
	for _, ts := range d.children {
		tools, err := ts.Tools(ctx)
		if err != nil {
			return nil, err
		}
		all = append(all, tools...)
	}

	// Session-specific tools.
	if ctx != nil {
		if sessionID := ctx.SessionID(); sessionID != "" {
			if sessionMap, ok := d.sessionChildren[sessionID]; ok {
				for _, ts := range sessionMap {
					tools, err := ts.Tools(ctx)
					if err != nil {
						return nil, err
					}
					all = append(all, tools...)
				}
			}
		}
	}

	return all, nil
}

// AddToolset registers a global child toolset by name. Replaces if name exists.
func (d *DynamicToolset) AddToolset(name string, ts tool.Toolset) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.children[name] = ts
}

// RemoveToolset unregisters a global child toolset by name.
func (d *DynamicToolset) RemoveToolset(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.children, name)
}

// HasToolset checks if a global toolset with the given name is registered.
func (d *DynamicToolset) HasToolset(name string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	_, ok := d.children[name]
	return ok
}

// AddSessionToolset registers a toolset visible only to the given session.
func (d *DynamicToolset) AddSessionToolset(sessionID, name string, ts tool.Toolset) {
	d.mu.Lock()
	defer d.mu.Unlock()
	m, ok := d.sessionChildren[sessionID]
	if !ok {
		m = make(map[string]tool.Toolset)
		d.sessionChildren[sessionID] = m
	}
	m[name] = ts
	d.sessionAccess[sessionID] = time.Now()
}

// RemoveSessionToolset removes a session-scoped toolset by name.
func (d *DynamicToolset) RemoveSessionToolset(sessionID, name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if m, ok := d.sessionChildren[sessionID]; ok {
		delete(m, name)
		if len(m) == 0 {
			delete(d.sessionChildren, sessionID)
		}
	}
}

// RemoveSessionSkillToolsets removes all skill toolsets (prefixed "skill:") for a session.
func (d *DynamicToolset) RemoveSessionSkillToolsets(sessionID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if m, ok := d.sessionChildren[sessionID]; ok {
		for name := range m {
			if len(name) > 6 && name[:6] == "skill:" {
				delete(m, name)
			}
		}
		if len(m) == 0 {
			delete(d.sessionChildren, sessionID)
		}
	}
}

// CleanupSession removes all session-scoped toolsets for the given session.
func (d *DynamicToolset) CleanupSession(sessionID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.sessionChildren, sessionID)
	delete(d.sessionAccess, sessionID)
}

// CleanupStaleSessions removes sessions not accessed for longer than maxAge.
// Returns the number of removed sessions.
func (d *DynamicToolset) CleanupStaleSessions(maxAge time.Duration) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for id, t := range d.sessionAccess {
		if t.Before(cutoff) {
			delete(d.sessionChildren, id)
			delete(d.sessionAccess, id)
			removed++
		}
	}
	return removed
}
