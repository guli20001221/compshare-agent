package workflow

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFinalCardStatesTheChargeTypeItNoLongerOffers guards a card that promises a
// control it does not have.
//
// Removing the editable ChargeType field left the description saying the billing
// mode "已在前面选定" — and there is no earlier card, because the charge type is
// settled from the create args before step one. A user reading that goes looking
// for a step that never existed. The card must instead NAME the value in force
// and say how to change it, which is by asking again.
func TestFinalCardStatesTheChargeTypeItNoLongerOffers(t *testing.T) {
	for _, tc := range []struct{ charge, label string }{
		{"", "按量付费（按小时计费）"}, // absent normalises to Postpay
		{"Spot", "抢占式"},
		{"Month", "包月"},
	} {
		t.Run(tc.charge, func(t *testing.T) {
			params := map[string]any{
				"GpuType": "A800", "Zone": "cn-wlcb-01",
				"Gpu": float64(1), "Cpu": float64(32), "Memory": float64(131072),
			}
			if tc.charge != "" {
				params["ChargeType"] = tc.charge
			}
			form, err := buildGuidedFinalForm(formWfCtx(t, params))
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
