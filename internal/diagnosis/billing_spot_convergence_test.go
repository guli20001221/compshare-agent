package diagnosis

import (
	"testing"

	"github.com/compshare-agent/internal/entity"
	"github.com/stretchr/testify/assert"
)

// The billing card and the resource projection must classify the same upstream
// row alike, including CHARGE_BY_SPOT rows with an empty ChargeType.
func TestProjectionAndBillingAgreeOnSpotFlag(t *testing.T) {
	rows := []map[string]any{
		{"UHostId": "uhost-spot", "ChargeType": "Postpay", "IsSpot": true},
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
