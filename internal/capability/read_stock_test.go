package capability

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/platform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func stockSupportZonesFixture() map[string]any {
	return map[string]any{"ZoneInfo": []any{
		map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "RegionId": float64(3001), "ZoneId": float64(1), "Describe": "华北二A"},
		map[string]any{"Zone": "cn-sh2-02", "Region": "cn-sh2", "RegionId": float64(3002), "ZoneId": float64(2), "Describe": "上海二B"},
	}}
}

func runStock(t *testing.T, exec ReadExecutor, fallbackGPUModel string, req StockAvailabilityRequest) ReadResult {
	t.Helper()
	reg := NewReadCapability(stockReadSpec())
	return reg.Run(context.Background(), req, ReadRuntime{Executor: exec, FallbackGPUModel: fallbackGPUModel})
}

func TestStockRequestHasNoRequiredFields(t *testing.T) {
	require.Nil(t, StockAvailabilityRequest{}.MissingFields())
}

// --- plain-listing render parity ------------------------------------------------

func TestStockRender_FilterDedupeAndStatus(t *testing.T) {
	raw := map[string]any{"AvailableInstanceTypes": []any{
		map[string]any{"Name": "4090", "Status": "Normal"},
		map[string]any{"Name": "4090", "Status": "Normal"}, // duplicate across zones
		map[string]any{"Name": "A100", "Status": "SoldOut"},
	}}
	reply := renderStockReply(raw, "4090 有货吗")
	assert.NotContains(t, reply, "A100", "filter excludes unmatched model")
	assert.Equal(t, 1, strings.Count(reply, "机型=4090"), "duplicates deduped")
	assert.Contains(t, reply, "不代表当前具体配置一定可创建", "Normal caveat present")
	assert.Contains(t, reply, soldOutDisclaimer)
}

// TestStockHandle_EmptyCatalog: no machine-type stock data is a structured Empty
// read (issue 1), short-circuited before the capacity precheck.
func TestStockHandle_EmptyCatalog(t *testing.T) {
	exec := &fakeReadExec{result: map[string]any{"AvailableInstanceTypes": []any{}}}

	result := runStock(t, exec, "", StockAvailabilityRequest{})

	require.Equal(t, platform.ReadStatusEmpty, result.Status)
	assert.Equal(t, noStockReply, result.Reply)
}

func TestStockRender_NoMatchAndEmpty(t *testing.T) {
	raw := map[string]any{"AvailableInstanceTypes": []any{map[string]any{"Name": "4090", "Status": "Normal"}}}
	assert.Contains(t, renderStockReply(raw, "H100"), "未在当前可售机型里找到您提到的型号")
	assert.Equal(t, noStockReply, renderStockReply(map[string]any{}, ""))
}

func TestStockEnvelope_SubjectsAndDisclaimer(t *testing.T) {
	raw := map[string]any{"AvailableInstanceTypes": []any{
		map[string]any{"Name": "4090", "Status": "Normal"},
		map[string]any{"Name": "A100", "Status": "SoldOut"},
	}}
	env := buildStockEnvelope(raw, "")
	assert.Equal(t, envelope.KindStockAvailability, env.Kind)
	require.Len(t, env.Subjects, 2)
	var disclaimer bool
	for _, f := range env.Computed {
		if f.Key == "disclaimer" {
			disclaimer = true
		}
	}
	assert.True(t, disclaimer, "envelope carries the sold-out disclaimer")
}

// --- RC017 referent parity ------------------------------------------------------

func TestStockReferentText_RC017(t *testing.T) {
	items := []any{map[string]any{"Name": "4090", "Status": "Normal"}}
	// explicit query is authoritative
	assert.Equal(t, "4090", stockReferentText(StockAvailabilityRequest{GPUType: "4090"}, "A100", items))
	// subject-eliding + prior model still offered → prior model is the referent
	assert.Equal(t, "4090", stockReferentText(StockAvailabilityRequest{}, "4090", items))
	// subject-eliding + prior model no longer offered → no referent
	assert.Equal(t, "", stockReferentText(StockAvailabilityRequest{}, "H100", items))
	// no query, no fallback → no referent
	assert.Equal(t, "", stockReferentText(StockAvailabilityRequest{}, "", items))
}

// --- handler: plain-listing path (no matched Normal entry) ----------------------

func TestStockHandle_PlainListingAttachesEnvelope(t *testing.T) {
	exec := &fakeReadExec{result: map[string]any{"AvailableInstanceTypes": []any{
		map[string]any{"Name": "4090", "Status": "Normal"},
		map[string]any{"Name": "A100", "Status": "SoldOut"},
	}}}

	// Empty query → no matched Normal entry → the capacity precheck falls through
	// to the plain catalog listing (single upstream call), with an envelope.
	result := runStock(t, exec, "", StockAvailabilityRequest{})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Equal(t, "DescribeAvailableCompShareInstanceTypes", result.ToolAction)
	require.Len(t, exec.calls, 1, "plain listing makes exactly one upstream call")
	assert.Contains(t, result.Reply, "机型=4090")
	require.NotNil(t, result.Envelope)
	assert.Equal(t, envelope.KindStockAvailability, result.Envelope.Kind)
}

// --- handler: full capacity-precheck orchestration ------------------------------

func TestStockHandle_CapacityPrecheckForMatchedNormalGPU(t *testing.T) {
	exec := &mapReadExec{results: map[string]map[string]any{
		"DescribeAvailableCompShareInstanceTypes": {
			"AvailableInstanceTypes": []any{
				map[string]any{
					"Name": "4090", "Zone": "cn-wlcb-01", "Status": "Normal",
					"Disks": []any{map[string]any{"BootDisk": []any{map[string]any{"Name": "CLOUD_SSD", "MinimalSize": float64(100)}}}},
				},
			},
		},
		"DescribeCompShareSupportZone": stockSupportZonesFixture(),
		"DescribeCompShareGpuInventory": {
			"GpuInventory": map[string]any{"Exclusive": map[string]any{"1": map[string]any{"4090": float64(0)}}},
		},
		"DescribeCompShareImages": {
			"ImageSet": []any{
				map[string]any{"CompShareImageId": "img-ubuntu", "Name": "Ubuntu-nvidia 22.04", "Status": "Available", "ImageType": "System"},
			},
		},
		"CheckCompShareResourceCapacity": {
			"Specs": []any{
				map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": false},
			},
		},
	}}

	result := runStock(t, exec, "", StockAvailabilityRequest{GPUType: "4090"})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Equal(t, "DescribeAvailableCompShareInstanceTypes", result.ToolAction)
	// The precheck RAN (call succeeded) but returned all ResourceEnough=false. That is a
	// SAMPLE negative (the probe used an arbitrary System image whose OS/disk feed the
	// sim), so the reply must report the on-sale truth and the sample outcome WITHOUT
	// generalizing to a global creation denial.
	assert.Contains(t, result.Reply, "样本容量预检未通过")
	assert.Contains(t, result.Reply, "机型状态：开售")
	assert.NotContains(t, result.Reply, "暂时不能新建实例", "a sample all-false must not be generalized to a global creation denial")
	assert.NotContains(t, result.Reply, "暂无可创建库存")
	assert.NotContains(t, result.Reply, "ResourceEnough", "no implementation details leak")
	require.Equal(t, []ReadEffect{RememberStockReferent{GPUModel: "4090"}}, result.Effects,
		"single matched model is remembered as a typed RC017 effect, not a shared-result field")
	assert.Nil(t, result.Envelope, "the capacity-precheck path carries no envelope (legacy parity)")

	// Full deterministic 5-call sequence.
	require.Len(t, exec.calls, 5)
	wantSeq := []string{
		"DescribeAvailableCompShareInstanceTypes",
		"DescribeCompShareSupportZone",
		"DescribeCompShareGpuInventory",
		"DescribeCompShareImages",
		"CheckCompShareResourceCapacity",
	}
	for i, want := range wantSeq {
		assert.Equal(t, want, exec.calls[i].action, "call %d", i)
	}
}

func TestStockHandle_UpstreamError(t *testing.T) {
	result := runStock(t, errReadExec{err: errors.New("boom")}, "", StockAvailabilityRequest{GPUType: "4090"})

	require.Equal(t, platform.ReadStatusFailureAfterTool, result.Status)
	assert.Equal(t, platform.ReadFailureGenericRead, result.FailureClass)
	assert.Equal(t, stockCapabilityLabel+": "+FriendlyReadFailureReply, result.Reply)
}

// TestSelectCapacityPrecheckImageID_KeywordFree is the F3 gate: the capacity-probe
// image pick must NOT prefer a name containing ubuntu/nvidia/cuda over the first
// Available image. The probe only needs SOME valid System image to satisfy the API's
// required CompShareImageId param; the deleted keyword scorer would have returned the
// ubuntu/cuda row here, so this would go red if the second image interpreter came back.
func TestSelectCapacityPrecheckImageID_KeywordFree(t *testing.T) {
	raw := map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "img-plain", "Name": "Base System", "ImageType": "System", "Status": "Available"},
		map[string]any{"CompShareImageId": "img-ubuntu", "Name": "Ubuntu 22.04 CUDA", "ImageType": "System", "Status": "Available"},
	}}
	assert.Equal(t, "img-plain", selectCapacityPrecheckImageID(raw),
		"probe must pick the first Available image, never prefer an ubuntu/cuda name (keyword scorer deleted)")
}

// TestSelectCapacityPrecheckImageID_SkipsUnusableAndEmpty pins the preserved contract:
// blank ids and non-Available/Normal statuses are skipped, and an empty set returns ""
// (honest — the caller then reports 容量预检未执行, never a defaulted image).
func TestSelectCapacityPrecheckImageID_SkipsUnusableAndEmpty(t *testing.T) {
	raw := map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "img-busy", "Name": "X", "Status": "Creating"},
		map[string]any{"CompShareImageId": "", "Name": "Y", "Status": "Available"},
		map[string]any{"CompShareImageId": "img-ok", "Name": "Z", "Status": "Normal"},
	}}
	assert.Equal(t, "img-ok", selectCapacityPrecheckImageID(raw), "first usable id after skipping busy + blank-id rows")
	assert.Equal(t, "", selectCapacityPrecheckImageID(map[string]any{"ImageSet": []any{}}),
		"no usable row must return \"\" (honest absence, never a default image)")
}

// TestRenderStockCapacity_AllPrecheckFailedIsUncertainNotSoldOut bounds the worst case
// of the capacity-probe image choice: even if the probe image errors every zone's
// precheck, a model the catalog reports as on-sale must degrade to "开售 / 未完成 /
// 暂不能确认", NEVER "售罄 / 暂无可创建库存". A precheck failure must not manufacture a
// false sold-out (the honest-degradation invariant referenced at read_stock.go's #3b).
func TestRenderStockCapacity_AllPrecheckFailedIsUncertainNotSoldOut(t *testing.T) {
	checks := []stockCapacityCheck{
		{Name: "4090", Zone: "cn-wlcb-01", Failed: true},
		{Name: "4090", Zone: "cn-bj2-03", Failed: true},
	}
	reply := renderStockInventoryCapacityReply(checks, "原始 GPU 库存：接口未返回数量。")
	assert.Contains(t, reply, "未完成")
	assert.Contains(t, reply, "开售")
	assert.NotContains(t, reply, "售罄")
	assert.NotContains(t, reply, "暂无可创建库存")
	assert.NotContains(t, reply, "暂时不能新建实例")
}

// TestRenderStockCapacity_SuccessButAllFalseIsNotGlobalDenial is the F3 sample-authority
// gate: a precheck that RAN (CheckedSpec>0, not Failed) but found no enough spec is a
// SAMPLE negative — the probe image's OS/boot-disk feed the scheduler sim, so it cannot
// prove the GPU has no stock. The reply must keep 机型开售 and must NOT emit a global
// creation denial. Only a POSITIVE precheck is authoritative. Goes red if the code
// generalizes an all-false sample into "暂时不能新建实例".
func TestRenderStockCapacity_SuccessButAllFalseIsNotGlobalDenial(t *testing.T) {
	checks := []stockCapacityCheck{
		{Name: "4090", Zone: "cn-wlcb-01", CheckedSpec: 2}, // ran, no EnoughSpecs, not Failed
	}
	reply := renderStockInventoryCapacityReply(checks, "原始 GPU 库存：接口未返回数量。")
	assert.Contains(t, reply, "开售")
	assert.Contains(t, reply, "样本")
	assert.NotContains(t, reply, "暂时不能新建实例", "an all-false sample must not become a global creation denial")
	assert.NotContains(t, reply, "暂无可创建库存")
	assert.NotContains(t, reply, "售罄")
}
