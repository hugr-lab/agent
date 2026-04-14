package agent

import (
	"fmt"
	"strings"
	"sync"

	"github.com/hugr-lab/hugen/interfaces"
)

// sessionPromptState holds per-session prompt state: active skill, instructions,
// catalog override, and appended references.
type sessionPromptState struct {
	activeSkill string   // currently loaded skill name
	skillInstr  string   // active skill instructions
	catalog     string   // session-specific catalog (overrides default)
	catalogSet  bool     // true if catalog was explicitly set/cleared for this session
	extras      []string // appended reference documents
}

// PromptBuilder assembles the system prompt from a global constitution plus
// per-session skill state. Each session has its own skill instructions, catalog,
// and references — parallel sessions never interfere with each other.
type PromptBuilder struct {
	mu             sync.RWMutex
	constitution   string
	defaultCatalog string // set at startup, shown to sessions that haven't set their own
	sessions       map[string]*sessionPromptState
}

// NewPromptBuilder creates a PromptBuilder with the base constitution text.
func NewPromptBuilder(constitution string) *PromptBuilder {
	return &PromptBuilder{
		constitution: constitution,
		sessions:     make(map[string]*sessionPromptState),
	}
}

// getSession returns existing session state or nil. Caller must hold lock.
func (p *PromptBuilder) getSession(sessionID string) *sessionPromptState {
	return p.sessions[sessionID]
}

// getOrCreateSession returns existing or creates new session state. Caller must hold write lock.
func (p *PromptBuilder) getOrCreateSession(sessionID string) *sessionPromptState {
	if s, ok := p.sessions[sessionID]; ok {
		return s
	}
	s := &sessionPromptState{}
	p.sessions[sessionID] = s
	return s
}

// SetDefaultCatalog sets the skill catalog shown to new sessions that haven't
// called SetCatalog yet. Called at startup with the initial skill list.
func (p *PromptBuilder) SetDefaultCatalog(skills []interfaces.SkillMeta) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.defaultCatalog = formatCatalog(skills)
}

// SetCatalog sets the skill catalog for a specific session.
func (p *PromptBuilder) SetCatalog(sessionID string, skills []interfaces.SkillMeta) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.getOrCreateSession(sessionID)
	s.catalog = formatCatalog(skills)
	s.catalogSet = true
}

// ClearCatalog removes the skill catalog for a specific session (after skill selection).
func (p *PromptBuilder) ClearCatalog(sessionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.getOrCreateSession(sessionID)
	s.catalog = ""
	s.catalogSet = true
}

// SetSkillInstructions sets the active skill's name and instructions for a session.
func (p *PromptBuilder) SetSkillInstructions(sessionID, skillName, instructions string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.getOrCreateSession(sessionID)
	s.activeSkill = skillName
	s.skillInstr = instructions
}

// AppendReference appends a reference document to the session's active skill.
func (p *PromptBuilder) AppendReference(sessionID, name, content string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.getOrCreateSession(sessionID)
	ref := fmt.Sprintf("## Reference: %s\n\n%s", name, content)
	s.extras = append(s.extras, ref)
}

// ClearSkill removes skill instructions and references for a session.
func (p *PromptBuilder) ClearSkill(sessionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.sessions[sessionID]; ok {
		s.activeSkill = ""
		s.skillInstr = ""
		s.extras = nil
	}
}

// ActiveSkill returns the currently loaded skill name for a session.
func (p *PromptBuilder) ActiveSkill(sessionID string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if s, ok := p.sessions[sessionID]; ok {
		return s.activeSkill
	}
	return ""
}

// BuildForSession assembles the full system prompt for a specific session.
// Uses session-specific catalog/skill/refs, falling back to defaultCatalog
// for sessions that haven't set their own catalog.
func (p *PromptBuilder) BuildForSession(sessionID string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var b strings.Builder
	b.WriteString(p.constitution)

	s := p.getSession(sessionID)

	// Catalog: use session-specific if set, otherwise default.
	catalog := p.defaultCatalog
	if s != nil && s.catalogSet {
		catalog = s.catalog
	}
	if catalog != "" {
		b.WriteString("\n\n")
		b.WriteString(catalog)
	}

	if s != nil {
		if s.skillInstr != "" {
			b.WriteString("\n\n")
			b.WriteString(s.skillInstr)
		}
		for _, extra := range s.extras {
			b.WriteString("\n\n")
			b.WriteString(extra)
		}
	}

	return b.String()
}

// CharCountForSession returns the total character count of the prompt for a session.
func (p *PromptBuilder) CharCountForSession(sessionID string) int {
	return len(p.BuildForSession(sessionID))
}

// CleanupSession removes all state for a session (call when session ends).
func (p *PromptBuilder) CleanupSession(sessionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sessions, sessionID)
}

// formatCatalog builds the catalog text from skill metadata.
func formatCatalog(skills []interfaces.SkillMeta) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Available Skills\n\n")
	for _, s := range skills {
		b.WriteString(fmt.Sprintf("- **%s**: %s", s.Name, s.Description))
		if len(s.Categories) > 0 {
			b.WriteString(fmt.Sprintf(" [%s]", strings.Join(s.Categories, ", ")))
		}
		b.WriteString("\n")
	}
	return b.String()
}
