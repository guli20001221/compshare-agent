package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestChargeTypeIsSettledBeforeEveryPoolScopedStep is the ordering invariant the
// Spot failure came down to, stated against what is ACTUALLY pool-scoped.
//
// It used to read "before any availability query" — two steps too strict, and
// that strictness is what kept the charge type off a card for so long.
// Measurement moved the line twice: the catalog query is not pool-scoped
// (InstanceType=spot returns an empty catalog, so it is no longer sent) and the
// inventory snapshot is not either (it carries BOTH pools and is fetched with no
// charge type). What IS pool-scoped starts at the GPU card.
//
// Any card offering an editable ChargeType must precede every step that consumes
// the pool. Placing one after them is the original bug: every card the user had
// already accepted would describe the other pool, and only 检查库存 would
// re-check — surfacing as a bare 库存不足 on a spec the cards showed as available.
func TestChargeTypeIsSettledBeforeEveryPoolScopedStep(t *testing.T) {
	steps := CreateInstanceGuidedDef().Steps
	// Named, not derived: adding a pool-scoped step means adding it here, and this
	// list is exactly the prompt to think about where the new step goes.
	poolScoped := map[string]bool{
		"选择 GPU": true, zoneCapacityStepName: true, "选择可用区": true, "查询容量规格": true,
	}
	firstPoolScoped := -1
	for i, s := range steps {
		if poolScoped[s.Name] {
			firstPoolScoped = i
			break
		}
	}
	require.NotEqual(t, -1, firstPoolScoped, "the guided flow must have a pool-scoped step")

	editable := -1
	for i, s := range steps {
		if s.Type != StepConfirm || s.BuildForm == nil {
			continue
		}
		form, err := s.BuildForm(formWfCtx(t, map[string]any{"GpuType": "A800"}))
		if err != nil || form == nil {
			continue // a card that cannot build here says nothing about ordering
		}
		if f := form.Field("ChargeType"); f != nil && f.Editable {
			assert.Less(t, i, firstPoolScoped,
				"card %q lets the user change ChargeType at step %d, but the pool is first "+
					"consumed at step %d", s.Name, i, firstPoolScoped)
			if editable == -1 {
				editable = i
			}
		}
	}
	assert.NotEqual(t, -1, editable,
		"the charge type must be askable on a card; leaving it to the create args alone was a "+
			"workaround for an ordering constraint that measurement has since removed")
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

// TestZoneCardNarrowsByPurchasePoolNotByPodness is the constraint running in its
// natural direction — the charge type is fixed first, so it narrows the zones
// rather than the other way round — and reading the RIGHT fact while doing it.
//
// The rule used to be "a pod zone cannot serve Spot". That is not true and it is
// not what the create gate enforces: 华北二C is a pod zone that DOES sell Spot
// (its Spot pool is exactly where its 4090 lives), while 华北一C is a pod zone
// that does not. A card built on pod-ness greys out a zone the gate would have
// accepted, which is the same class of error as offering one it will refuse.
func TestZoneCardNarrowsByPurchasePoolNotByPodness(t *testing.T) {
	// The decisive case: a POD zone whose Spot pool is real. The old pod-ness rule
	// returned true here and hid a creatable zone.
	spotPod, _ := poolUnsupportedInZone(guidedInventoryContext(t, "cn-wlcb-03", "Spot"), "cn-wlcb-03", "4090")
	assert.False(t, spotPod, "华北二C sells Spot — pod-ness is not the question")

	// And the same zone under a mode it does NOT sell must still be refused, so
	// the gate is not simply always-false now.
	exclusivePod, reason := poolUnsupportedInZone(guidedInventoryContext(t, "cn-wlcb-03", "Postpay"), "cn-wlcb-03", "4090")
	assert.True(t, exclusivePod, "华北二C has no Exclusive pool for 4090")
	assert.Contains(t, reason, "独占")

	spotExclusiveOnlyPod, reason := poolUnsupportedInZone(guidedInventoryContext(t, "cn-bj2-03", "Spot"), "cn-bj2-03", "4090")
	assert.True(t, spotExclusiveOnlyPod, "华北一C is exclusive-only")
	assert.Contains(t, reason, "抢占式")

	normal, _ := poolUnsupportedInZone(guidedInventoryContext(t, "cn-wlcb-01", "Postpay"), "cn-wlcb-01", "4090")
	assert.False(t, normal, "a normal zone serves the exclusive modes")

	// An unresolvable zone is not evidence: the gate stays silent and
	// validateCreatePlacement remains the authoritative refusal.
	unresolvable, _ := poolUnsupportedInZone(guidedInventoryContext(t, "cn-wlcb-01", "Spot"), "cn-nowhere-99", "4090")
	assert.False(t, unresolvable, "an unresolvable placement must not be read as a refusal")
}

// TestGPUCardDisablesAModelOnlyWhenNoZoneSellsThePool is the aggregate the GPU
// card has to use: it runs BEFORE the zone card, so a model spans zones and one
// surviving zone keeps it buyable.
func TestGPUCardDisablesAModelOnlyWhenNoZoneSellsThePool(t *testing.T) {
	// 华北二C sells 4090 on Spot, 华北一C does not. Under Spot the model is still
	// buyable, so the card must not grey it out.
	mixed, _ := poolUnsupportedEverywhere(
		guidedInventoryContext(t, "cn-wlcb-03", "Spot"), []string{"cn-bj2-03", "cn-wlcb-03"}, "4090")
	assert.False(t, mixed, "one zone that still sells the pool keeps the model selectable")

	// Every listed zone refuses the pool — now it is a real dead end.
	none, reason := poolUnsupportedEverywhere(
		guidedInventoryContext(t, "cn-bj2-03", "Spot"), []string{"cn-bj2-03"}, "4090")
	assert.True(t, none)
	assert.Contains(t, reason, "该机型不支持抢占式")

	// A model with no zones at all is not evidence of anything.
	empty, _ := poolUnsupportedEverywhere(guidedInventoryContext(t, "cn-bj2-03", "Spot"), nil, "4090")
	assert.False(t, empty)
}
