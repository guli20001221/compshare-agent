package readprojection

import (
	"testing"

	"github.com/compshare-agent/internal/entity"
	"github.com/stretchr/testify/assert"
)

func spotInstance(id string, isSpot bool) entity.InstanceSnapshot {
	return entity.InstanceSnapshot{
		UHostId: id, Name: "amazon-pyabsa", State: "Running", OsType: "LINUX",
		GPU: 4, GpuType: "4090", CPU: 64, Memory: 262144,
		Zone: "cn-sh2-02", Region: "cn-sh2",
		ChargeType: "Postpay", IsSpot: isSpot, AutoRenew: "Yes",
	}
}

// Both rows describe as Postpay; is_spot must preserve the independent resource
// mode for positive and negative answers.
func TestSpotInstanceIsVisibleInTheEnvelope(t *testing.T) {
	env := BuildResourceEnvelope([]entity.InstanceSnapshot{
		spotInstance("uhost-spot", true),
		spotInstance("uhost-postpay", false),
	})

	assertEnvelopeFact(t, env, "uhost-spot", "charge_type", "Postpay")
	assertEnvelopeFact(t, env, "uhost-spot", "is_spot", "是")
	assertEnvelopeFact(t, env, "uhost-postpay", "charge_type", "Postpay")
	assertEnvelopeFact(t, env, "uhost-postpay", "is_spot", "否")
}

// A spot resource line must name its product mode instead of ordinary postpay.
func TestSpotInstanceRendersAsSpotNotAsPostpay(t *testing.T) {
	spotLine := RenderResourceSummary([]entity.InstanceSnapshot{spotInstance("uhost-spot", true)},
		ResourceEnvelopeMeta{TotalCount: 1})
	assert.Contains(t, spotLine, "抢占式")
	assert.NotContains(t, spotLine, ChargeTypeLabel("Postpay"),
		"抢占式 replaces the ChargeType word; printing both invites the same conflation")

	postpayLine := RenderResourceSummary([]entity.InstanceSnapshot{spotInstance("uhost-postpay", false)},
		ResourceEnvelopeMeta{TotalCount: 1})
	assert.Contains(t, postpayLine, ChargeTypeLabel("Postpay"))
	assert.NotContains(t, postpayLine, "抢占式")
}
