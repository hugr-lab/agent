package planner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/hugr-lab/hugen/pkg/missions/graph"
	"github.com/hugr-lab/hugen/pkg/skills"
)

// RoleEntry is one (skill, role) pair the planner can cite. Listed
// verbatim in the planner prompt; unknown entries in the LLM's output
// fail validation.
type RoleEntry struct {
	Skill       string
	Role        string
	Description string
}

// BuildRoleCatalog extracts every (skill, role) pair declared across
// the loaded skills. Order is stable (skill then role, lexicographic)
// so the planner's instruction block is deterministic and cache-key
// stable.
func BuildRoleCatalog(loaded []*skills.Skill) []RoleEntry {
	var out []RoleEntry
	for _, sk := range loaded {
		if sk == nil {
			continue
		}
		for role, spec := range sk.SubAgents {
			out = append(out, RoleEntry{
				Skill:       sk.Name,
				Role:        role,
				Description: strings.TrimSpace(spec.Description),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Skill != out[j].Skill {
			return out[i].Skill < out[j].Skill
		}
		return out[i].Role < out[j].Role
	})
	return out
}

// defaultPromptHeader is the embedded fallback used when the runtime
// hasn't supplied a header (typically: unit tests with no filesystem).
// Production loads the prose from skills/_coordinator/planner-prompt.md
// so operators can edit the planning rules without rebuilding.
const defaultPromptHeader = `You plan a mission graph for the coordinator. Return EXACTLY one JSON object with keys ` + "`missions`" + ` (array) and ` + "`edges`" + ` (array). Each mission has ` + "`{id, skill, role, task}`" + ` where ` + "`id`" + ` is a sequential integer 1..N and ` + "`skill`" + ` and ` + "`role`" + ` reference a registered (skill, role) pair from the list below. Each edge has ` + "`{from, to}`" + ` referencing mission ids; it means ` + "`from`" + ` must reach status ` + "`done`" + ` before ` + "`to`" + ` can start. Do not emit prose, markdown, or any keys other than those listed.`

// buildInstruction renders the operator-supplied instruction header
// followed by the dynamic role catalog. Header is verbatim; catalog
// formatting + the empty-catalog refuse rule live in code so the
// strict JSON contract stays stable across operator edits.
func buildInstruction(header string, catalog []RoleEntry) string {
	var b bytes.Buffer
	b.WriteString(strings.TrimSpace(header))
	b.WriteString("\n\nRegistered specialist roles (skill : role — description):\n")
	if len(catalog) == 0 {
		b.WriteString("  (none — the coordinator has no specialist roles loaded; refuse the plan by returning an empty missions array.)\n")
	}
	for _, r := range catalog {
		fmt.Fprintf(&b, "  %s : %s — %s\n", r.Skill, r.Role, r.Description)
	}
	return b.String()
}

// BuildPrompt concatenates the instruction block with the user's
// goal. Returns one string ready to ship as the prompt body; caller
// wraps it in the ADK LLMRequest. Empty header falls back to the
// embedded default (unit-test path).
func BuildPrompt(header string, catalog []RoleEntry, goal string) string {
	if strings.TrimSpace(header) == "" {
		header = defaultPromptHeader
	}
	var b bytes.Buffer
	b.WriteString(buildInstruction(header, catalog))
	b.WriteString("\nGoal to plan:\n  ")
	b.WriteString(strings.TrimSpace(goal))
	b.WriteString("\n")
	return b.String()
}

// ParseResponse applies the strict JSON contract. Returns ErrPlanParse
// on any malformed output; specific Err* on semantic validation
// failures.
func ParseResponse(raw string, catalog []RoleEntry) (graph.PlanResult, error) {
	text := strings.TrimSpace(raw)
	// Some models wrap JSON in ```json fences; strip once.
	if strings.HasPrefix(text, "```") {
		if idx := strings.Index(text, "\n"); idx >= 0 {
			text = text[idx+1:]
		}
		text = strings.TrimSuffix(text, "```")
		text = strings.TrimSpace(text)
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	var out graph.PlannerOutput
	if err := dec.Decode(&out); err != nil {
		return graph.PlanResult{}, fmt.Errorf("%w: %v", graph.ErrPlanParse, err)
	}
	if dec.More() {
		return graph.PlanResult{}, fmt.Errorf("%w: trailing content", graph.ErrPlanParse)
	}
	if len(out.Missions) == 0 {
		return graph.PlanResult{}, graph.ErrNoMissions
	}

	known := map[string]struct{}{}
	for _, r := range catalog {
		known[r.Skill+"/"+r.Role] = struct{}{}
	}
	for _, m := range out.Missions {
		if strings.TrimSpace(m.Task) == "" {
			return graph.PlanResult{}, fmt.Errorf("%w: mission %d", graph.ErrEmptyTask, m.ID)
		}
		if _, ok := known[m.Skill+"/"+m.Role]; !ok {
			return graph.PlanResult{}, fmt.Errorf("%w: %s/%s", graph.ErrUnknownRole, m.Skill, m.Role)
		}
	}

	plan := graph.PlanResult{Missions: out.Missions, Edges: out.Edges}
	if err := graph.ValidatePlan(plan); err != nil {
		return graph.PlanResult{}, err
	}
	return plan, nil
}

// SkillsDigest produces a stable hash over the loaded skills' (name,
// version) pairs. Order-independent — sorts before hashing. Shared
// with cache-key construction.
func SkillsDigest(loaded []*skills.Skill) string {
	keys := make([]string, 0, len(loaded))
	for _, sk := range loaded {
		if sk == nil {
			continue
		}
		keys = append(keys, sk.Name+"@"+sk.Version)
	}
	sort.Strings(keys)
	return hashSHA256(strings.Join(keys, ","))
}
