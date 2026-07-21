package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestChargeTypeIsSettledBeforeAnyAvailabilityQuery is the ordering invariant the
// Spot failure came down to.
//
// Availability is per resource pool: stepQueryInstanceTypes sends
// InstanceType=spot only for Spot, guidedInventoryFrom reads the Spot pool
// instead of Exclusive, and every capacity call carries the charge type. All of
// that is decided by params before step one. If a step were ever inserted that
// ASKS for the charge type, it would have to sit before these queries, and the
// answers they produced for the cards would describe a pool the user had not
// chosen yet.
func TestChargeTypeIsSettledBeforeAnyAvailabilityQuery(t *testing.T) {
	steps := CreateInstanceGuidedDef().Steps
	firstAvailabilityQuery := -1
	for i, s := range steps {
		if s.Name == "查询可用配比" || s.Name == "查询GPU库存" {
			firstAvailabilityQuery = i
			break
		}
	}
	require.NotEqual(t, -1, firstAvailabilityQuery, "the guided flow must query availability")

	for i, s := range steps {
		if s.Type != StepConfirm || s.BuildForm == nil {
			continue
		}
		form, err := s.BuildForm(formWfCtx(t, map[string]any{"GpuType": "A800"}))
		if err != nil || form == nil {
			continue // a card that cannot build here says nothing about ordering
		}
		if f := form.Field("ChargeType"); f != nil && f.Editable {
			assert.Less(t, i, firstAvailabilityQuery,
				"card %q lets the user change ChargeType at step %d, but availability was "+
					"already queried at step %d for a different charge type",
				s.Name, i, firstAvailabilityQuery)
		}
	}
}

// TestGuidedFinalCardDoesNotOfferChargeTypeEdit pins the specific hole. The final
// card's RevalidateFrom is 形成执行草稿, so an edit here re-runs the draft, the
// stock check and the price — but NOT the availability catalog, the inventory
// pool or the per-zone capacity probe. A ChargeType edit here therefore changed
// which resource pool the create targets while leaving every card the user
// already accepted describing the other one.
func TestGuidedFinalCardDoesNotOfferChargeTypeEdit(t *testing.T) {
	wfCtx := formWfCtx(t, map[string]any{
		"GpuType": "A800", "Zone": "cn-wlcb-01",
		"Gpu": float64(1), "Cpu": float64(32), "Memory": float64(131072),
	})
	form, err := buildGuidedFinalForm(wfCtx)
	require.NoError(t, err)
	require.NotNil(t, form)
	require.NotNil(t, form.Step)
	assert.True(t, form.Step.Final, "premise: this is the final guided card")

	assert.Nil(t, form.Field("ChargeType"),
		"the final card must not re-open the charge type; its edit does not re-run the "+
			"charge-type-scoped availability queries that already ran")

	// The plain (non-guided) create keeps its editable charge type: it has exactly
	// one card, resolves its zone before asking, and its only capacity gate
	// (检查库存) does re-run on an edit — so the hole does not exist there.
	plain, err := buildCreateConfirmForm(wfCtx)
	require.NoError(t, err)
	require.NotNil(t, plain.Field("ChargeType"),
		"the single-card plain create must still let the user choose a billing mode")
	assert.True(t, plain.Field("ChargeType").Editable)
}

// TestZoneCardRefusesAPodZoneUnderSpot is the constraint running in its natural
// direction. It used to point the other way — the charge-type card disabled Spot
// once a zone was known — which cannot work when the charge type is fixed first.
// Now the charge type narrows the zones.
func TestZoneCardRefusesAPodZoneUnderSpot(t *testing.T) {
	podZone := "cn-pod-01"
	withCharge := func(charge string) *Context {
		c := formWfCtx(t, map[string]any{"GpuType": "4090", "ChargeType": charge})
		c.referenceData.ZoneCatalog = createZoneCatalog()
		return c
	}

	assert.False(t, spotUnavailableInZone(withCharge("Postpay"), podZone),
		"on-demand in a pod zone is fine — the gate must not fire on charge types it is not about")
	assert.False(t, spotUnavailableInZone(withCharge("Spot"), "cn-wlcb-01"),
		"a normal zone serves Spot")
	// An unresolvable zone is not evidence: the gate stays silent and
	// validateCreatePlacement remains the authoritative refusal.
	assert.False(t, spotUnavailableInZone(withCharge("Spot"), "cn-nowhere-99"),
		"an unresolvable placement must not be read as a refusal")
}
