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

// TestBillingFactsSpotDetectedViaIsSpotFlag: upstream renders a spot instance's
// ChargeType as "Postpay" (or, under the CHARGE_BY_SPOT enum, empty) and flags it
// ONLY with IsSpot=true — it never emits ChargeType "Spot". The chain must key off
// IsSpot so spot is counted/priced as hourly and labelled 抢占式, and (crucially) an
// empty-ChargeType spot is not silently dropped from the hourly totals.
func TestBillingFactsSpotDetectedViaIsSpotFlag(t *testing.T) {
	spotRunning := billingFactHostSpot("uhost-spot", "spot-run", "Running", "Postpay", true, 0.80, 0.05, 0, "4090", 1)
	summary := BuildBillingFacts([]any{spotRunning})
	require.Len(t, summary.Instances, 1)
	fact := summary.Instances[0]
	assert.True(t, fact.IsSpot)
	assert.Equal(t, "hour", fact.Period)
	assert.True(t, summary.HasDynamic)
	assert.Equal(t, 0.80, fact.ActualComputeCharge) // running → full unit price

	// stopped spot → ¥0 compute (关机不计费), same as postpay
	stoppedSpot := billingFactHostSpot("uhost-spot2", "spot-stop", "Stopped", "Postpay", true, 0.80, 0.05, 0, "4090", 1)
	s2 := BuildBillingFacts([]any{stoppedSpot})
	assert.Equal(t, 0.0, s2.Instances[0].ActualComputeCharge)

	// label surfaces as 抢占式/时, not 按量/时
	conclusion, _ := buildBillingSummary([]any{spotRunning})
	assert.Contains(t, conclusion, "抢占式/时")

	// robustness: spot billed under CHARGE_BY_SPOT → ChargeType "" but IsSpot=true
	// must STILL count as hourly (not dropped from totals, not mislabelled prepaid).
	emptyCharge := billingFactHostSpot("uhost-spot3", "spot-empty", "Running", "", true, 0.80, 0.05, 0, "4090", 1)
	s3 := BuildBillingFacts([]any{emptyCharge})
	assert.True(t, s3.HasDynamic, "spot with empty ChargeType must still be hourly via IsSpot")
	assert.False(t, s3.HasPrepaid)
}

func billingFactHostSpot(id, name, state, chargeType string, isSpot bool, instancePrice, diskPrice, imagePrice float64, gpuType string, gpu float64) map[string]any {
	h := billingFactHost(id, name, state, chargeType, instancePrice, diskPrice, imagePrice, gpuType, gpu)
	h["IsSpot"] = isSpot
	return h
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
