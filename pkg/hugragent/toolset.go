// Package hugragent implements the custom HugrAgent loop with dynamic tool management.
package hugragent

import (
	"sync"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/tool"
)

// DynamicToolset is a thread-safe tool.Toolset that allows adding and removing
// child toolsets at runtime. Tools() merges all children on every call,
// so the LLM sees an up-to-date tool list each turn.
type DynamicToolset struct {
	mu       sync.RWMutex
	children map[string]tool.Toolset
}

var _ tool.Toolset = (*DynamicToolset)(nil)

// NewDynamicToolset creates an empty DynamicToolset.
func NewDynamicToolset() *DynamicToolset {
	return &DynamicToolset{
		children: make(map[string]tool.Toolset),
	}
}

func (d *DynamicToolset) Name() string { return "dynamic" }

// Tools merges tools from all child toolsets.
func (d *DynamicToolset) Tools(ctx agent.ReadonlyContext) ([]tool.Tool, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var all []tool.Tool
	for _, ts := range d.children {
		tools, err := ts.Tools(ctx)
		if err != nil {
			return nil, err
		}
		all = append(all, tools...)
	}
	return all, nil
}

// AddToolset registers a child toolset by name. Replaces if name exists.
func (d *DynamicToolset) AddToolset(name string, ts tool.Toolset) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.children[name] = ts
}

// RemoveToolset unregisters a child toolset by name.
func (d *DynamicToolset) RemoveToolset(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.children, name)
}

// HasToolset checks if a toolset with the given name is registered.
func (d *DynamicToolset) HasToolset(name string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	_, ok := d.children[name]
	return ok
}
