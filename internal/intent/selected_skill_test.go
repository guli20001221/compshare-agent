package intent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeriveSelectedSkills_IntentBackedObserveOnly(t *testing.T) {
	tests := []struct {
		name string
		plan Plan
		want []SelectedSkill
	}{
		{
			name: "capability intent derives registry skill",
			plan: Plan{Intent: IntentPricingQuery},
			want: []SelectedSkill{{Name: "pricing_query", Resolution: SkillResolutionDerivedFromIntent}},
		},
		{
			name: "agent arm derives deploy skill",
			plan: Plan{Intent: IntentDeployModel},
			want: []SelectedSkill{{Name: "deploy_model", Resolution: SkillResolutionAgentArm}},
		},
		{
			name: "diagnosis is not plan-time selected",
			plan: Plan{Intent: IntentDiagnosis},
			want: []SelectedSkill{{Resolution: SkillResolutionResolvedInReAct}},
		},
		{
			name: "plain resource intent has no skill projection",
			plan: Plan{Intent: IntentResourceInfo},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, DeriveSelectedSkills(tt.plan))
		})
	}
}

func TestWithDerivedSelectedSkills_OverridesPlannerSuppliedSkills(t *testing.T) {
	plan := Plan{
		Intent: IntentPricingQuery,
		Skills: []SelectedSkill{
			{Name: "deploy_model", Resolution: "planner_supplied"},
		},
	}

	got := withDerivedSelectedSkills(plan)

	require.Len(t, got.Skills, 1)
	assert.Equal(t, SelectedSkill{Name: "pricing_query", Resolution: SkillResolutionDerivedFromIntent}, got.Skills[0])
}
