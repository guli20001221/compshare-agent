package intent

import "github.com/compshare-agent/internal/routing"

// DeriveSelectedSkills projects today's intent-first routing contract into the
// observe-only Plan.Skills field. It deliberately ignores any planner-supplied
// skills so R0 cannot change dispatch or trust model-selected skill names.
func DeriveSelectedSkills(plan Plan) []SelectedSkill {
	if name, ok := routeNameForIntent(plan.Intent); ok {
		return []SelectedSkill{{Name: name, Resolution: SkillResolutionDerivedFromIntent}}
	}
	switch plan.Intent {
	case IntentDeployModel:
		return []SelectedSkill{{Name: "deploy_model", Resolution: SkillResolutionAgentArm}}
	case IntentDiagnosis:
		return []SelectedSkill{{Resolution: SkillResolutionResolvedInReAct}}
	default:
		return nil
	}
}

func withDerivedSelectedSkills(plan Plan) Plan {
	plan.Skills = DeriveSelectedSkills(plan)
	return plan
}

func routeNameForIntent(i Intent) (string, bool) {
	for _, route := range routing.GeneratedRoutes() {
		if route.IntentLabel != "" && Intent(route.IntentLabel) == i {
			return route.Name, true
		}
	}
	return "", false
}
