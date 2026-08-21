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

// TestSpotInstanceIsVisibleInTheEnvelope pins the fix for the 2026-08-17 turn where
// the Agent read charge_type=Postpay off a 抢占式 instance and answered 「所以它不是抢占式
// 实例」. Both rows describe as Postpay; only is_spot separates them, so the fact has to
// be there — and has to be there when false too, or "不是抢占式" is still a guess.
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

// TestSpotInstanceRendersAsSpotNotAsPostpay: the rendered line is the other place a
// customer reads the billing mode. ChargeType alone printed 按量付费 for a spot box.
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
