package intent

import (
	"testing"

	"github.com/compshare-agent/internal/entity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// alwaysHitResolver resolves every id/name to a HIT. The guard test below is
// about planner JSON shape, not entity resolution, so the resolver must succeed
// when an example carries a target.
type alwaysHitResolver struct{}

func (alwaysHitResolver) ResolveByID(id string) (*entity.InstanceSnapshot, entity.ResolveResult) {
	return &entity.InstanceSnapshot{}, entity.ResolveResult{Status: entity.ResolveHit, Query: id}
}

func (alwaysHitResolver) ResolveByName(name string) ([]*entity.InstanceSnapshot, entity.ResolveResult) {
	return []*entity.InstanceSnapshot{{}}, entity.ResolveResult{Status: entity.ResolveHit, Query: name}
}

func (alwaysHitResolver) InstanceIDTokensInText(text string) []string {
	return nil
}

// TestPlannerExamples_UseSlimPlannerOutput locks the router-v2 contract:
// planner examples teach only the fields that still drive routing. Tool windows,
// retrieval, and hard-block hints are now backend-derived, so they must not
// re-enter few-shot JSON.
func TestPlannerExamples_UseSlimPlannerOutput(t *testing.T) {
	groups := routerPromptExampleGroups()
	require.NotEmpty(t, groups, "no planner example groups loaded")
	for _, group := range groups {
		for _, ex := range group.Examples {
			for _, deprecated := range []string{"required_tools", "retrieval", "hard_block_hint"} {
				assert.NotContainsf(t, ex.PlanJSON, deprecated, "few-shot %q must not emit deprecated planner field", ex.Question)
			}
			plan, err := parsePlanJSON(ex.PlanJSON)
			require.NoErrorf(t, err, "few-shot %q (intent %s) PlanJSON does not parse", ex.Question, group.Intent)
			// UserText = the example's own question so provenance source_span
			// checks pass (each few-shot's source_span is a substring of its
			// question). Resolver always hits so target validation can't mask the
			// shape check.
			verr := ValidateRoute(plan, ValidationContext{
				UserText: ex.Question,
				Resolver: alwaysHitResolver{},
			})
			assert.NoErrorf(t, verr, "few-shot %q must validate after planner-output slimming", ex.Question)
		}
	}
}
