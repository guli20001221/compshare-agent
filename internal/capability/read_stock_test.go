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
	assert.Contains(t, result.Reply, "默认创建配置暂未通过容量预检")
	assert.Contains(t, result.Reply, "机型状态：开售")
	assert.NotContains(t, result.Reply, "ResourceEnough", "no implementation details leak")
	assert.Equal(t, "4090", result.ResolvedStockGPUModel, "single matched model is recorded (RC017)")
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
