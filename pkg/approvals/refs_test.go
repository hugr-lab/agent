package approvals

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRisk_Validate(t *testing.T) {
	cases := []struct {
		r       Risk
		wantErr bool
	}{
		{RiskLow, false},
		{RiskMedium, false},
		{RiskHigh, false},
		{Risk("urgent"), true},
		{Risk(""), true},
	}
	for _, c := range cases {
		t.Run(string(c.r), func(t *testing.T) {
			err := c.r.Validate()
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestStatus_Validate_IsTerminal(t *testing.T) {
	cases := []struct {
		s          Status
		wantErr    bool
		isTerminal bool
	}{
		{StatusPending, false, false},
		{StatusApproved, false, true},
		{StatusRejected, false, true},
		{StatusModified, false, true},
		{StatusExpired, false, true},
		{Status("unknown"), true, false},
	}
	for _, c := range cases {
		t.Run(string(c.s), func(t *testing.T) {
			err := c.s.Validate()
			if c.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, c.isTerminal, c.s.IsTerminal())
		})
	}
}

func TestDecision_Validate(t *testing.T) {
	cases := []struct {
		d       Decision
		wantErr bool
	}{
		{DecisionApprove, false},
		{DecisionReject, false},
		{DecisionModify, false},
		{DecisionAnswer, false},
		{Decision(""), true},
		{Decision("unknown"), true},
	}
	for _, c := range cases {
		t.Run(string(c.d), func(t *testing.T) {
			err := c.d.Validate()
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestMatchAny(t *testing.T) {
	cases := []struct {
		tool     string
		patterns []string
		want     bool
	}{
		{"data-execute_mutation", []string{"data-execute_mutation"}, true},
		{"data-execute_mutation", []string{"data-*"}, true},
		{"data-execute_mutation", []string{"*"}, true},
		{"data-execute_mutation", []string{"data-list"}, false},
		{"data-execute_mutation", []string{"python-*"}, false},
		{"x", []string{}, false},
	}
	for _, c := range cases {
		t.Run(c.tool, func(t *testing.T) {
			assert.Equal(t, c.want, matchAny(c.tool, c.patterns))
		})
	}
}
