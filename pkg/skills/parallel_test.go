package skills

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParallelValidationSpec_Validate(t *testing.T) {
	cases := []struct {
		name    string
		spec    *ParallelValidationSpec
		wantErr bool
	}{
		{"nil spec OK", nil, false},
		{"disabled OK", &ParallelValidationSpec{Enabled: false, MergeStrategy: MergeStrategyMerge}, false},
		{"enabled + agent_choice OK", &ParallelValidationSpec{Enabled: true, MergeStrategy: MergeStrategyAgentChoice}, false},
		{"enabled + empty defaults to agent_choice", &ParallelValidationSpec{Enabled: true}, false},
		{"enabled + user_choice errors", &ParallelValidationSpec{Enabled: true, MergeStrategy: MergeStrategyUserChoice}, true},
		{"enabled + merge errors", &ParallelValidationSpec{Enabled: true, MergeStrategy: MergeStrategyMerge}, true},
		{"enabled + unknown errors", &ParallelValidationSpec{Enabled: true, MergeStrategy: "voodoo"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.spec.Validate()
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
