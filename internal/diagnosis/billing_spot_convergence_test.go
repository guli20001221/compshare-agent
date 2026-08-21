package diagnosis

import (
	"testing"

	"github.com/compshare-agent/internal/entity"
	"github.com/stretchr/testify/assert"
)

// TestSpotIsDecidedOnceForBothReaders is the regression for 2026-08-17, where one
// turn produced two contradictory answers about ONE upstream row: the billing card
// (which reads IsSpot) rendered 抢占式/时, while the instance projection (which read
// only ChargeType) said Postpay — and the Agent, seeing only the projection, told the
// customer the box was not 抢占式.
//
// The assertion is deliberately not "both return true". It is that the two readers
// return the SAME value for the same row, across the shapes where they could drift:
// a strict bool, the string spelling this API uses for its other booleans, and the
// CHARGE_BY_SPOT row that carries no ChargeType at all. Two separate implementations
// of one rule is what let them disagree; they now share entity.InstanceIsSpot, and
// this test fails if anything re-forks that decision.
func TestSpotIsDecidedOnceForBothReaders(t *testing.T) {
	rows := []map[string]any{
		{"UHostId": "uhost-spot", "ChargeType": "Postpay", "IsSpot": true},
		{"UHostId": "uhost-spot-str", "ChargeType": "Postpay", "IsSpot": "true"},
		{"UHostId": "uhost-spot-empty", "ChargeType": "", "IsSpot": true},
		{"UHostId": "uhost-postpay", "ChargeType": "Postpay", "IsSpot": false},
		{"UHostId": "uhost-month", "ChargeType": "Month"},
	}
	for _, row := range rows {
		id, _ := row["UHostId"].(string)
		t.Run(id, func(t *testing.T) {
			projected := entity.InstanceFromMap(row).IsSpot
			billing := billingInstanceFact(row).IsSpot
			assert.Equal(t, projected, billing,
				"the instance projection and the billing chain disagree about whether %s is 抢占式; "+
					"a customer would be told both at once, in the same turn", id)

			// And the label the customer reads follows that one decision.
			if billing {
				assert.Equal(t, "抢占式/时", chargeTypeLabel(billingInstanceFact(row).ChargeType, billing))
			}
		})
	}
}
