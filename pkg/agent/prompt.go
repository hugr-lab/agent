package agent

import (
	"fmt"
	"strings"
	"sync"

	"github.com/hugr-lab/hugen/interfaces"
)

// PromptBuilder assembles the system prompt from constitution, active skill
// instructions, and skill catalog. It is updated when skills are loaded/unloaded.
type PromptBuilder struct {
	mu           sync.RWMutex
	constitution string
	skillInstr   string // active skill instructions (empty until skill-load)
	catalog      string // skill catalog text (set from skill-list, cleared after skill-load)
	extras       []string
}

// NewPromptBuilder creates a PromptBuilder with the base constitution text.
func NewPromptBuilder(constitution string) *PromptBuilder {
	return &PromptBuilder{constitution: constitution}
}

// Build assembles the full system prompt.
func (p *PromptBuilder) Build() string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var b strings.Builder
	b.WriteString(p.constitution)

	if p.catalog != "" {
		b.WriteString("\n\n")
		b.WriteString(p.catalog)
	}

	if p.skillInstr != "" {
		b.WriteString("\n\n")
		b.WriteString(p.skillInstr)
	}

	for _, extra := range p.extras {
		b.WriteString("\n\n")
		b.WriteString(extra)
	}

	return b.String()
}

// SetCatalog sets the skill catalog text (injected when LLM needs to pick a skill).
func (p *PromptBuilder) SetCatalog(skills []interfaces.SkillMeta) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(skills) == 0 {
		p.catalog = ""
		return
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
	p.catalog = b.String()
}

// ClearCatalog removes the skill catalog from the prompt (called after skill selection).
func (p *PromptBuilder) ClearCatalog() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.catalog = ""
}

// SetSkillInstructions sets the active skill's instructions.
func (p *PromptBuilder) SetSkillInstructions(instructions string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.skillInstr = instructions
}

// AppendReference appends a reference document to the active skill instructions.
func (p *PromptBuilder) AppendReference(name, content string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ref := fmt.Sprintf("## Reference: %s\n\n%s", name, content)
	p.extras = append(p.extras, ref)
}

// ClearSkill removes skill instructions and references (for skill unload).
func (p *PromptBuilder) ClearSkill() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.skillInstr = ""
	p.extras = nil
}

// CharCount returns the total character count of the current prompt.
func (p *PromptBuilder) CharCount() int {
	return len(p.Build())
}
