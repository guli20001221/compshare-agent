package diagnosis

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBillingFactsRunningDynamicUsesInstancePrice(t *testing.T) {
	summary := BuildBillingFacts([]any{billingFactHost("uhost-run", "running", "Running", "Dynamic", 1.58, 0.05, 0, "4090", 1)})

	require.Len(t, summary.Instances, 1)
	fact := summary.Instances[0]
	assert.Equal(t, "uhost-run", fact.UHostID)
	assert.Equal(t, "running", fact.Name)
	assert.Equal(t, "Running", fact.State)
	assert.Equal(t, "Dynamic", fact.ChargeType)
	assert.Equal(t, "hour", fact.Period)
	assert.Equal(t, 1.58, fact.ActualComputeCharge)
	assert.Equal(t, 0.0, fact.RetainedStoppedCharge)
	assert.InDelta(t, 1.63, summary.HourlyTotal, 0.0001)
	assert.True(t, summary.HasDynamic)
	assert.False(t, summary.HasPrepaid)
	assert.Equal(t, 1, summary.RunningCount)
}

func TestBillingFactsStoppedDynamicRetainsDiskAndImageOnly(t *testing.T) {
	summary := BuildBillingFacts([]any{billingFactHost("uhost-stop", "stopped", "Stopped", "Postpay", 1.58, 0.05, 0.30, "4090", 1)})

	require.Len(t, summary.Instances, 1)
	fact := summary.Instances[0]
	assert.Equal(t, 0.0, fact.ActualComputeCharge)
	assert.Equal(t, 0.35, fact.RetainedStoppedCharge)
	assert.InDelta(t, 0.35, summary.HourlyTotal, 0.0001)
	assert.InDelta(t, 0.35, summary.StoppedRetainedTotal, 0.0001)
	assert.Equal(t, 1, summary.StoppedCount)
	assert.True(t, summary.HasDynamic)
}

func TestBillingFactsPrepaidPreservesChargeTypeAndDoesNotPretendStoppedFree(t *testing.T) {
	summary := BuildBillingFacts([]any{billingFactHost("uhost-day", "prepaid", "Stopped", "Day", 5.00, 0.10, 0, "A100", 2)})

	require.Len(t, summary.Instances, 1)
	fact := summary.Instances[0]
	assert.Equal(t, "Day", fact.ChargeType)
	assert.Equal(t, "day", fact.Period)
	assert.Equal(t, 5.00, fact.InstancePrice)
	assert.Equal(t, 5.00, fact.ActualComputeCharge)
	assert.InDelta(t, 0.10, fact.RetainedStoppedCharge, 0.0001)
	assert.InDelta(t, 0.0, summary.HourlyTotal, 0.0001)
	assert.True(t, summary.HasPrepaid)
	assert.False(t, summary.HasDynamic)
}

func TestBillingFactsMixedInstancesComputesTotals(t *testing.T) {
	summary := BuildBillingFacts([]any{
		billingFactHost("uhost-run", "running", "Running", "Dynamic", 1.58, 0.05, 0, "4090", 1),
		billingFactHost("uhost-stop", "stopped", "Stopped", "Dynamic", 1.58, 0.05, 0.30, "4090", 1),
		billingFactHost("uhost-day", "prepaid", "Stopped", "Day", 5.00, 0.10, 0, "A100", 2),
	})

	require.Len(t, summary.Instances, 3)
	assert.InDelta(t, 1.98, summary.HourlyTotal, 0.0001)
	assert.InDelta(t, 0.45, summary.StoppedRetainedTotal, 0.0001)
	assert.Equal(t, 1, summary.RunningCount)
	assert.Equal(t, 2, summary.StoppedCount)
	assert.True(t, summary.HasDynamic)
	assert.True(t, summary.HasPrepaid)
}

func TestBillingFactsMatchExistingSummaryForMixedInstances(t *testing.T) {
	hosts := []any{
		billingFactHost("uhost-run", "running", "Running", "Dynamic", 1.58, 0.05, 0, "4090", 1),
		billingFactHost("uhost-stop", "stopped", "Stopped", "Dynamic", 1.58, 0.05, 0.30, "4090", 1),
		billingFactHost("uhost-day", "prepaid", "Stopped", "Day", 5.00, 0.10, 0, "A100", 2),
	}

	conclusion, suggestion := buildBillingSummary(hosts)

	assert.Contains(t, conclusion, "3 个实例")
	assert.Contains(t, conclusion, "按量/抢占式实例合计: ¥1.98/时")
	assert.Contains(t, conclusion, "关机实例（2 个）仍在产生磁盘和镜像保留费用，合计 ¥0.45/时")
	assert.Contains(t, conclusion, "包月/包日实例按预付费计费")
	assert.Contains(t, suggestion, "释放")
}

func billingFactHost(id, name, state, chargeType string, instancePrice, diskPrice, imagePrice float64, gpuType string, gpu float64) map[string]any {
	return map[string]any{
		"UHostId":             id,
		"Name":                name,
		"State":               state,
		"ChargeType":          chargeType,
		"InstancePrice":       instancePrice,
		"DiskPrice":           diskPrice,
		"CompShareImagePrice": imagePrice,
		"GpuType":             gpuType,
		"GPU":                 gpu,
	}
}
