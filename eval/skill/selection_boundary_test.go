package skill_eval

import (
	"testing"

	"github.com/compshare-agent/internal/intent"
	"github.com/stretchr/testify/assert"
)

func TestSelectionCaseBoundaryPassesExpectedIntentAndForbiddenSkill(t *testing.T) {
	c := SkillCase{
		ID:              "boundary_git_hub_accel",
		Lane:            laneBoundary,
		ExpectedIntent:  string(intent.IntentKnowledgeQA),
		ForbiddenSkills: []string{"network_accelerator_status"},
	}

	pass := evaluateSelectionCase(c, intent.Plan{Intent: intent.IntentKnowledgeQA}, "")
	assert.True(t, pass.Hit)
	assert.True(t, pass.BoundaryHit)
	assert.Equal(t, string(intent.IntentKnowledgeQA), pass.ExpectedIntent)
	assert.Empty(t, pass.SelectedSkill)

	fail := evaluateSelectionCase(c, intent.Plan{Intent: intent.Intent("network_accelerator_status")}, "network_accelerator_status")
	assert.False(t, fail.Hit)
	assert.False(t, fail.BoundaryHit)
	assert.Equal(t, "network_accelerator_status", fail.SelectedSkill)
}
