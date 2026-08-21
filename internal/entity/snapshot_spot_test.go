package entity

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Spot and ordinary postpay instances can share the same ChargeType; IsSpot is
// therefore a separate projection fact.
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
	assert.False(t, InstanceFromMap(map[string]any{"UHostId": "uhost-x", "IsSpot": "true"}).IsSpot,
		"the upstream Boolean contract must not be widened from an unrelated field's wire spelling")
}
