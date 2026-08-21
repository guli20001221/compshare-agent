package entity

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestInstanceSnapshotCarriesTheSpotFlag: upstream reports a 抢占式 instance as
// ChargeType "Postpay" (or "" under CHARGE_BY_SPOT) plus IsSpot=true, so the
// snapshot must carry the flag separately. Parsing ChargeType alone is what made
// spot and 按量 the same value everywhere downstream.
func TestInstanceSnapshotCarriesTheSpotFlag(t *testing.T) {
	spot := InstanceFromMap(map[string]any{
		"UHostId": "uhost-spot", "ChargeType": "Postpay", "IsSpot": true,
	})
	assert.Equal(t, "Postpay", spot.ChargeType, "upstream never spells the ChargeType 'Spot'")
	assert.True(t, spot.IsSpot)

	emptyChargeType := InstanceFromMap(map[string]any{
		"UHostId": "uhost-spot2", "ChargeType": "", "IsSpot": true,
	})
	assert.True(t, emptyChargeType.IsSpot, "CHARGE_BY_SPOT rows carry no ChargeType at all")

	onDemand := InstanceFromMap(map[string]any{
		"UHostId": "uhost-postpay", "ChargeType": "Postpay", "IsSpot": false,
	})
	assert.False(t, onDemand.IsSpot)

	assert.False(t, InstanceFromMap(map[string]any{"UHostId": "uhost-x"}).IsSpot,
		"an absent key is not spot")
}

// TestInstanceIsSpotAcceptsTheWireSpellingsThisAPIMixes: the sibling boolean on the
// same row, AutoRenew, arrives as "Yes"/"No", so a string-valued IsSpot is a shape
// this upstream already produces. A strict bool assertion would read it as false —
// i.e. would silently report a spot instance as 按量, the exact failure being fixed.
func TestInstanceIsSpotAcceptsTheWireSpellingsThisAPIMixes(t *testing.T) {
	for _, raw := range []any{true, "true", "True", " yes ", "YES", "1", 1, int64(1), 1.0, json.Number("1")} {
		assert.True(t, InstanceIsSpot(map[string]any{"IsSpot": raw}), "value %#v", raw)
	}
	for _, raw := range []any{false, "false", "No", "", "0", 0, 0.0, json.Number("0"), nil, "maybe"} {
		assert.False(t, InstanceIsSpot(map[string]any{"IsSpot": raw}), "value %#v", raw)
	}
}
