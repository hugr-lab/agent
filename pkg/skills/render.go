package skills

import (
	"fmt"
	"strings"
)

// RenderCatalog builds the "## Available Skills" prompt block.
func RenderCatalog(skills []SkillMeta) string {
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

// RenderInstructions formats a loaded skill's instructions into a prompt block,
// listing the tool names the skill contributes and the reference
// documents that are available vs already loaded for the current session.
//
// Rendering the refs status on every turn lets the model pick the right
// skill_ref next step when a data tool errors out, without re-loading a
// ref it has already read.
func RenderInstructions(sk *Skill, tools []string, loadedRefs []string) string {
	var b strings.Builder
	b.WriteString("## Skill: ")
	b.WriteString(sk.Name)
	b.WriteString("\n\n")
	b.WriteString(sk.Instructions)

	if len(tools) > 0 {
		b.WriteString("\n\n### Available tools\n\n")
		for _, t := range tools {
			b.WriteString("- `")
			b.WriteString(t)
			b.WriteString("`\n")
		}
	}

	if len(sk.Refs) > 0 {
		loaded := make(map[string]bool, len(loadedRefs))
		for _, n := range loadedRefs {
			loaded[n] = true
		}
		b.WriteString("\n\n### References (call `skill_ref` to load)\n\n")
		for _, r := range sk.Refs {
			b.WriteString("- ")
			if loaded[r.Name] {
				b.WriteString("**[LOADED]** `")
			} else {
				b.WriteString("`")
			}
			b.WriteString(r.Name)
			b.WriteString("`")
			if r.Description != "" {
				b.WriteString(" — ")
				b.WriteString(strings.TrimSpace(r.Description))
			}
			b.WriteString("\n")
		}
	}

	return b.String()
}

// RenderReference formats a reference document as a prompt block.
func RenderReference(refName, content string) string {
	return fmt.Sprintf("## Reference: %s\n\n%s", refName, content)
}
