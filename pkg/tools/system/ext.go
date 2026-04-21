package system

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/sessions"
	"github.com/hugr-lab/hugen/pkg/skills"
)

// catalogLister is the extension interface satisfied by *sessions.Session
// so skill_list can fetch the skill catalogue without system holding a
// direct reference to skills.Manager.
type catalogLister interface {
	ListSkills(ctx context.Context) ([]skills.SkillMeta, error)
}

// skillDescriptor exposes skill metadata that isn't in *sessions.Session
// directly. Implemented by *sessions.Session via its SkillMeta method.
type skillDescriptor interface {
	SkillMeta(ctx context.Context, skillName string) sessions.SkillDescriptorMeta
}

// refReader returns raw reference-document content (for the skill_ref
// tool response body — the content is already injected into the prompt
// by sess.LoadReference).
type refReader interface {
	ReadReference(ctx context.Context, skill, ref string) (string, error)
}
