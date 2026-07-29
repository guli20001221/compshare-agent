package workflow

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/deployment"
)

// TestFinalCardStatesTheChargeTypeItNoLongerOffers guards a card that promises a
// control it does not have.
//
// The final card cannot change the billing mode (a late switch desyncs the pool
// every earlier step queried), so it must NAME the value in force and say where
// it CAN be changed. Where that is depends on whether this run showed the
// purchase-mode card — the sentence used to be a constant that always said
// "重新发起创建", which was written before the card existed and afterwards told
// users to redo the whole request to change something they had just been asked.
func TestFinalCardStatesTheChargeTypeItNoLongerOffers(t *testing.T) {
	// Every case here is one the USER named, which is what skips the card and
	// leaves no earlier step to point at. The un-named case takes the other
	// branch and is covered by the test below.
	for _, tc := range []struct{ charge, label string }{
		{"Postpay", "按量付费（按小时计费）"},
		{"Spot", "抢占式"},
		{"Month", "包月"},
	} {
		t.Run(tc.charge, func(t *testing.T) {
			wfCtx := formWfCtx(t, map[string]any{
				"GpuType": "A800", "Zone": "cn-wlcb-01",
				"Gpu": float64(1), "Cpu": float64(32), "Memory": float64(131072),
				"ChargeType": tc.charge,
			})
			wfCtx.referenceData.ChargeTypeUserPinned = true
			require.True(t, guidedStepSkipped(wfCtx, guidedStepChargeType), "premise: no earlier step")

			form, err := buildGuidedFinalForm(wfCtx)
			require.NoError(t, err)
			require.NotNil(t, form.Step)

			assert.Nil(t, form.Field("ChargeType"), "premise: the field is gone")
			assert.Contains(t, form.Step.Description, tc.label,
				"the card must name the billing mode actually in force")
			assert.NotContains(t, form.Step.Description, "已在前面选定",
				"there is no earlier step to point at; saying so sends the user hunting")
			assert.True(t,
				strings.Contains(form.Step.Description, "重新发起创建"),
				"…and must say how to change it, since the card cannot")
		})
	}
}

// The other half: when the run DID show the purchase-mode card, telling the user
// to start over is wrong — the step they want is right behind them.
func TestFinalCardPointsBackAtTheChargeTypeCardWhenItWasShown(t *testing.T) {
	wfCtx := formWfCtx(t, map[string]any{
		"GpuType": "A800", "Zone": "cn-wlcb-01",
		"Gpu": float64(1), "Cpu": float64(32), "Memory": float64(131072),
		// The Agent's default, not the user's word — so the card was shown.
		"ChargeType": "Postpay",
	})
	require.False(t, guidedStepSkipped(wfCtx, guidedStepChargeType), "premise: the card was shown")

	form, err := buildGuidedFinalForm(wfCtx)
	require.NoError(t, err)
	require.NotNil(t, form.Step)

	assert.Contains(t, form.Step.Description, "按量付费（按小时计费）",
		"the card must still name the billing mode in force")
	assert.NotContains(t, form.Step.Description, "重新发起创建",
		"the user was offered a purchase-mode card; sending them to redo the request is wrong")
	assert.Contains(t, form.Step.Description, "购买方式",
		"point back at the step that owns the choice")
}

// TestChargeTypeChangeHintNeverOffersTheModeAlreadyInForce is the gate on the
// sentence a real 抢占式 request produced: "需要包日、包月或抢占式，请重新发起创建并直接
// 说明（例如「用抢占式创建一台…」）" — the way to change AWAY from 抢占式 was 抢占式, and
// the example was verbatim what the user had just typed. A card that answers a
// request by quoting it back gives the user no way to tell it was understood.
//
// The example is checked by PARSING it, not by matching text: the whole promise
// of "例如「用X创建一台…」" is that retyping it works, so X has to resolve — and to
// something other than the mode already in force.
// It iterates createFormChargeTypes rather than a hand-listed set, so a billing
// mode added to the product is covered the day it appears. A hardcoded list would
// make this test a patch on the four modes that existed when it was written.
func TestChargeTypeChangeHintNeverOffersTheModeAlreadyInForce(t *testing.T) {
	require.NotEmpty(t, createFormChargeTypes, "premise: there are modes to iterate")
	for _, mode := range createFormChargeTypes {
		t.Run(mode.Value, func(t *testing.T) {
			wfCtx := formWfCtx(t, map[string]any{
				"GpuType": "A800", "Zone": "cn-wlcb-01",
				"Gpu": float64(1), "Cpu": float64(32), "Memory": float64(131072),
				"ChargeType": mode.Value,
			})
			wfCtx.referenceData.ChargeTypeUserPinned = true
			require.True(t, guidedStepSkipped(wfCtx, guidedStepChargeType), "premise: the user named the mode")

			hint := chargeTypeChangeHint(wfCtx)
			assert.NotContains(t, hint, mode.Label,
				"%s is in force; offering it as the way to change away from it is not an option", mode.Value)

			example := chargeTypeHintExample(t, hint)
			resolved, ok := deployment.ExplicitChargeTypeFromPhrase(example)
			require.True(t, ok, "the example %q must be a phrase the server resolves, or retyping it silently loses the pin", example)
			assert.NotEqual(t, mode.Value, resolved,
				"the example must ask for a DIFFERENT mode than the one in force")
		})
	}
}

// Every selectable billing mode needs a phrase the parser accepts, or the hint
// degrades to a sentence with no example at all — silently, on the mode that was
// added last. This is the check that makes the hint generic rather than correct
// for exactly today's four modes.
func TestEverySelectableChargeTypeHasATypeablePhrase(t *testing.T) {
	for _, mode := range createFormChargeTypes {
		phrase, ok := deployment.ExplicitChargeTypePhrase(mode.Value)
		require.True(t, ok, "%s has no phrase a user could type", mode.Value)
		resolved, ok := deployment.ExplicitChargeTypeFromPhrase(phrase)
		require.True(t, ok, "%s: phrase %q does not parse back", mode.Value, phrase)
		assert.Equal(t, mode.Value, resolved, "%s: phrase %q resolves to a different mode", mode.Value, phrase)
	}
}

// chargeTypeHintExample lifts X out of 例如「用X创建一台…」.
func chargeTypeHintExample(t *testing.T, hint string) string {
	t.Helper()
	_, after, ok := strings.Cut(hint, "例如「用")
	require.True(t, ok, "hint must carry an example: %q", hint)
	example, _, ok := strings.Cut(after, "创建一台")
	require.True(t, ok, "hint example must be phrased as 用X创建一台: %q", hint)
	return example
}

// TestChargeTypeLabelReusesTheOptionLabels keeps one name per billing mode. Two
// hand-written label sets drift, and a card that calls Postpay something the
// selectable option does not is a discrepancy the user has no way to resolve.
func TestChargeTypeLabelReusesTheOptionLabels(t *testing.T) {
	for _, opt := range createFormChargeTypes {
		assert.Equal(t, opt.Label, chargeTypeLabel(opt.Value),
			"%s must render as the label its option already carries", opt.Value)
	}
	assert.Equal(t, "SomethingNew", chargeTypeLabel("SomethingNew"),
		"an unrecognised mode shows itself rather than being silently relabelled")
}
