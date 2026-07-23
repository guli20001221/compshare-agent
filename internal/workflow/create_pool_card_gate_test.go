package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// poolCardCatalog is the availability catalog the two guided cards read. 4090 is
// offered in all three zones; under Spot only 华北二C actually sells it, which is
// what makes the zone card's per-zone answer and the GPU card's aggregate differ.
func poolCardCatalog() map[string]any {
	row := func(zone string) map[string]any {
		return map[string]any{"Name": "4090", "Status": "Normal", "Zone": zone}
	}
	return map[string]any{"AvailableInstanceTypes": []any{
		row("cn-wlcb-01"), row("cn-bj2-03"), row("cn-wlcb-03"),
	}}
}

func poolCardOption(opts []ConfirmFormOption, value string) (ConfirmFormOption, bool) {
	for _, opt := range opts {
		if opt.Value == value {
			return opt, true
		}
	}
	return ConfirmFormOption{}, false
}

// TestZoneCardDisablesOnlyZonesThatDoNotSellTheChosenPool wires the predicate to
// the card. The predicate having the right answer is not enough — the card has to
// ask it, and nothing else covered that the option's Disabled flag is set.
func TestZoneCardDisablesOnlyZonesThatDoNotSellTheChosenPool(t *testing.T) {
	wfCtx := guidedInventoryContext(t, "cn-wlcb-03", "Spot")
	wfCtx.StepResults["查询可用配比"] = poolCardCatalog()

	_, opts, _ := guidedZoneFormOptions(wfCtx, poolCardCatalog(), "4090", "", wfCtx.Params, wfCtx.Result(createGPUInventoryStep))
	require.NotEmpty(t, opts)

	sellsSpot, ok := poolCardOption(opts, "cn-wlcb-03")
	require.True(t, ok, "华北二C must be offered at all")
	assert.False(t, sellsSpot.Disabled, "华北二C sells Spot; disabling it hides a zone the create gate accepts")

	exclusiveOnly, ok := poolCardOption(opts, "cn-bj2-03")
	require.True(t, ok)
	assert.True(t, exclusiveOnly.Disabled, "华北一C has no Spot pool")
	assert.Contains(t, exclusiveOnly.Reason, "抢占式")
	assert.NotContains(t, exclusiveOnly.Note, "库存快照",
		"a refused zone must not also quote a stock number from the pool it cannot sell")
}

// TestGPUCardDisablesAModelNoZoneSellsInTheChosenPool is the same wiring for the
// card that runs first, where only the aggregate over zones is answerable.
func TestGPUCardDisablesAModelNoZoneSellsInTheChosenPool(t *testing.T) {
	// Spot: 华北二C still sells 4090, so the model stays selectable.
	mixed := guidedInventoryContext(t, "cn-wlcb-03", "Spot")
	_, opts := guidedGPUFormOptions(mixed, poolCardCatalog(), nil, "", false, mixed.Params, mixed.Result(createGPUInventoryStep))
	opt, ok := poolCardOption(opts, "4090")
	require.True(t, ok)
	assert.False(t, opt.Disabled, "one zone that still sells Spot keeps 4090 selectable")

	// Restrict the catalog to the two zones that do NOT sell Spot: now every zone
	// the model is offered in refuses the pool, and the card must say so.
	deadEnd := guidedInventoryContext(t, "cn-bj2-03", "Spot")
	catalog := map[string]any{"AvailableInstanceTypes": []any{
		map[string]any{"Name": "4090", "Status": "Normal", "Zone": "cn-bj2-03"},
	}}
	deadEnd.StepResults["查询可用配比"] = catalog
	_, opts = guidedGPUFormOptions(deadEnd, catalog, nil, "", false, deadEnd.Params, deadEnd.Result(createGPUInventoryStep))
	opt, ok = poolCardOption(opts, "4090")
	require.True(t, ok)
	assert.True(t, opt.Disabled, "no zone sells 4090 on Spot here")
	assert.Contains(t, opt.Reason, "该机型不支持抢占式")
	assert.NotContains(t, opt.Note, "库存快照",
		"a refused model must not also quote the other pool's stock")
}
