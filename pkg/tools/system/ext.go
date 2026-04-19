package system

import (
	"context"

	"github.com/hugr-lab/hugen/interfaces"
)

// catalogLister is the extension interface satisfied by pkg/session.Session
// so skill_list can fetch the skill catalogue without system holding a
// direct reference to skills.Manager.
type catalogLister interface {
	ListSkills(ctx context.Context) ([]interfaces.SkillMeta, error)
}

// SkillDescriptorMeta is what skill_load needs to shape its response —
// references with descriptions + the author-provided workflow hint.
type SkillDescriptorMeta struct {
	Refs     []interfaces.SkillRefMeta
	NextStep string
}

// skillDescriptor exposes skill metadata that isn't in interfaces.Session
// directly. Implemented by pkg/session.Session.
type skillDescriptor interface {
	SkillMeta(ctx context.Context, skillName string) SkillDescriptorMeta
}

// refReader returns raw reference-document content (for the skill_ref
// tool response body — the content is already injected into the prompt
// by sess.LoadReference).
type refReader interface {
	ReadReference(ctx context.Context, skill, ref string) (string, error)
}
