package intent

import (
	"testing"

	"github.com/compshare-agent/internal/entity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// alwaysHitResolver resolves every id/name to a HIT. The guard test below is
// about the required_tools allowlist, not entity resolution, so the resolver
// must SUCCEED — otherwise a target-carrying few-shot would fail ValidatePlan
// for an unrelated reason (target not in registry) and mask the tool check.
type alwaysHitResolver struct{}

func (alwaysHitResolver) ResolveByID(id string) (*entity.InstanceSnapshot, entity.ResolveResult) {
	return &entity.InstanceSnapshot{}, entity.ResolveResult{Status: entity.ResolveHit, Query: id}
}

func (alwaysHitResolver) ResolveByName(name string) ([]*entity.InstanceSnapshot, entity.ResolveResult) {
	return []*entity.InstanceSnapshot{{}}, entity.ResolveResult{Status: entity.ResolveHit, Query: name}
}

// TestPlannerExamples_TaughtRequiredToolsAreAccepted locks the one real
// invariant that the operation_lifecycle 8/8 schema_invalid bug (2026-05-30,
// both flash and pro) violated: a tool the few-shot TEACHES the model to emit
// in plan.required_tools must be an ACCEPTED required tool for that intent —
// i.e. every few-shot's required_tools must pass ValidatePlan, which checks
// requiredToolsForIntent.
//
// This is deliberately NARROW. It does NOT assert validator == tool_subset:
// those are different semantics —
//   - requiredToolsForIntent (validator) = what the planner may DECLARE in
//     plan.required_tools (narrow: read tool used to resolve/list)
//   - IntentToolSubset (tool_subset.go) = what tools the model may SEE/CALL for
//     the intent (wide: includes mutating *Workflow tools)
//
// The correct relationship is validator ⊆ tool_subset, not equality. Asserting
// equality here would (rightly) fail.
//
// Root cause this closes: TestPlannerPromptExamplesGroupedByIntentWithSource
// only compared few-shot required_tools to its OWN expectedTools map and never
// called ValidatePlan, so the planner.go few-shots (which teach
// ["DescribeCompShareInstance"] for operation_lifecycle) drifted away from the
// validator's requiredToolsForIntent (which had no operation_lifecycle case)
// without any test failing. Memory: cross-pr-contract-drift-check.
func TestPlannerExamples_TaughtRequiredToolsAreAccepted(t *testing.T) {
	groups := plannerPromptExampleGroups()
	require.NotEmpty(t, groups, "no planner example groups loaded")
	for _, group := range groups {
		for _, ex := range group.Examples {
			plan, err := parsePlanJSON(ex.PlanJSON)
			require.NoErrorf(t, err, "few-shot %q (intent %s) PlanJSON does not parse", ex.Question, group.Intent)
			// UserText = the example's own question so provenance source_span
			// checks pass (each few-shot's source_span is a substring of its
			// question). Resolver always hits so target validation can't be the
			// failing cause — only the required_tools allowlist can.
			verr := ValidatePlan(plan, ValidationContext{
				UserText: ex.Question,
				Resolver: alwaysHitResolver{},
			})
			assert.NoErrorf(t, verr,
				"few-shot %q teaches required_tools=%v for intent %s but ValidatePlan rejects it; "+
					"the taught tool must be an accepted required tool — fix requiredToolsForIntent(%s), not the few-shot",
				ex.Question, plan.RequiredTools, group.Intent, group.Intent)
		}
	}
}
