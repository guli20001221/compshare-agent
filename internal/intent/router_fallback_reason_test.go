package intent

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// On real 6.26-7.9 production traffic, 6.0% of FOLLOW-UP turns fall back with
// route_status=fallback_invalid, against 1.0% of opening turns — a 6x multi-turn
// degradation, and 39% of those turns then do zero tool work. Nothing in the system
// could explain it, because the trace projection erased both halves of the answer:
// it overwrites Intent with `unknown` and never recorded the validation code at all.
//
// That erasure makes two OPPOSITE bugs look identical in the trace:
//
//	the user asked something genuinely off-platform            -> nothing to fix
//	we REJECTED a correct route on a provenance technicality   -> fix the validator
//
// Both read `intent: unknown, route_status: fallback_invalid`. This pins that a
// rejected plan now carries the intent the model actually chose and the reason we
// refused it.
func TestRejectedPlanKeepsTheIntentAndTheReasonItWasRefused(t *testing.T) {
	// The real shape of the failure: mid-diagnosis, the user types their own instance
	// id. The model routes it correctly to diagnosis; the entity validator refuses the
	// target_ref because the registry was never populated on this HTTP session.
	trace := ProjectPlannerTrace(IntentRouterResult{
		Plan:                unknownFallbackPlan(),
		Fallback:            true,
		Attempts:            2,
		LastValidationCode:  ErrEntityNotFound,
		LastValidationField: "slots.target_refs[0].value",
		LastRejectedIntent:  IntentDiagnosis,
	}, PlannerTraceOptions{Enabled: true, Model: "ds-v4-flash"})

	require.False(t, trace.SchemaValid, "a rejected plan is not schema-valid")
	assert.Equal(t, string(IntentUnknown), trace.Intent,
		"the dispatch-facing intent still collapses to unknown — routing is unchanged by this")

	// ...but the trace no longer pretends the user was off-platform.
	assert.Equal(t, string(IntentDiagnosis), trace.RejectedIntent,
		"the model picked diagnosis; a trace that only says `unknown` blames the user "+
			"for a rejection we caused")
	assert.Equal(t, string(ErrEntityNotFound), trace.ValidationCode)
	assert.Equal(t, "slots.target_refs[0].value", trace.ValidationField)
	assert.Equal(t, 2, trace.Attempts, "it burned every retry before giving up")

	// Enum + schema path only, no user text. This has to be safe to leave ON in
	// production, because production is the only place the question is answerable.
	raw, err := json.Marshal(trace)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "\"validation_code\":\"entity_not_found\"")
	assert.Contains(t, string(raw), "\"rejected_intent\":\"diagnosis\"")
}

// A model that emits unparseable JSON and a model that emits a well-formed plan we
// then refuse are different diseases with opposite fixes — schema-compliance breakdown
// (the 2026-05-28 PriorText avalanche signature) versus an over-strict validator. Both
// land in fallback_invalid, so the bucket is only useful if they are labelled apart.
func TestParseFailureIsLabelledApartFromAValidatorRejection(t *testing.T) {
	parseFail := ProjectPlannerTrace(IntentRouterResult{
		Plan:               unknownFallbackPlan(),
		Fallback:           true,
		Attempts:           2,
		LastValidationCode: ErrUnparseableJSON,
	}, PlannerTraceOptions{Enabled: true})

	assert.Equal(t, string(ErrUnparseableJSON), parseFail.ValidationCode)
	assert.Empty(t, parseFail.RejectedIntent,
		"there is no rejected intent when the output never parsed — do not invent one")

	rejected := ProjectPlannerTrace(IntentRouterResult{
		Plan:               unknownFallbackPlan(),
		Fallback:           true,
		LastValidationCode: ErrAttemptedHallucinatedEntity,
		LastRejectedIntent: IntentOperationLifecycle,
	}, PlannerTraceOptions{Enabled: true})

	assert.NotEqual(t, parseFail.ValidationCode, rejected.ValidationCode,
		"if these two collapse to one label, fallback_invalid stays undiagnosable")
}

// A plan that VALIDATES must not carry a stale rejection from an earlier attempt: a
// turn that needed one retry and then succeeded is a clean turn, not a failed one.
// Getting this wrong would inflate the very metric the fix is judged on.
func TestASuccessfulRetryCarriesNoRejection(t *testing.T) {
	trace := ProjectPlannerTrace(IntentRouterResult{
		Plan: IntentRoute{
			SchemaVersion: SchemaVersion,
			Intent:        IntentResourceInfo,
			Confidence:    0.85,
		},
		Attempts: 2,
	}, PlannerTraceOptions{Enabled: true})

	require.True(t, trace.SchemaValid)
	assert.Empty(t, trace.ValidationCode, "a turn that recovered is not a failed turn")
	assert.Empty(t, trace.RejectedIntent)
	assert.Equal(t, string(IntentResourceInfo), trace.Intent)
}
