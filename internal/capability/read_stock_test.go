package capability

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/platform"
	"github.com/compshare-agent/internal/zones"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func stockSupportZonesFixture() map[string]any {
	return map[string]any{"ZoneInfo": []any{
		map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "RegionId": float64(3001), "ZoneId": float64(1), "Describe": "华北二A"},
		map[string]any{"Zone": "cn-sh2-02", "Region": "cn-sh2", "RegionId": float64(3002), "ZoneId": float64(2), "Describe": "上海二B"},
		map[string]any{"Zone": "cn-bj2-03", "Region": "cn-bj2", "RegionId": float64(3003), "ZoneId": float64(5001), "Describe": "华北一C", "IsPod": true},
		map[string]any{"Zone": "cn-wlcb-03", "Region": "cn-wlcb", "RegionId": float64(1000039), "ZoneId": float64(10033), "Describe": "华北二C", "IsPod": true},
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
	assert.Contains(t, result.Reply, "机型开售")
	assert.NotContains(t, result.Reply, "暂时不能新建实例", "a sample all-false must not be generalized to a global creation denial")
	assert.NotContains(t, result.Reply, "暂无可创建库存")
	assert.NotContains(t, result.Reply, "ResourceEnough", "no implementation details leak")
	require.Equal(t, []ReadEffect{RememberStockReferent{GPUModel: "4090"}}, result.Effects,
		"single matched model is remembered as a typed RC017 effect, not a shared-result field")
	assert.Nil(t, result.Envelope, "the capacity-precheck path carries no envelope (legacy parity)")

	// Raw GPU-count inventory is not an authority for user-facing creatability;
	// the deterministic path uses the sale catalog plus a capacity precheck.
	require.Len(t, exec.calls, 6)
	wantSeq := []string{
		"DescribeAvailableCompShareInstanceTypes",
		"DescribeCompShareSupportZone",
		"DescribeCompShareGpuInventory",
		"DescribeCompShareGpuInventory",
		"DescribeCompShareImages",
		"CheckCompShareResourceCapacity",
	}
	for i, want := range wantSeq {
		assert.Equal(t, want, exec.calls[i].action, "call %d", i)
	}
}

type stockDualInventoryExec struct {
	*mapReadExec
	official map[string]any
	pod      map[string]any
}

func (e *stockDualInventoryExec) ExecuteInternal(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	if action != "DescribeCompShareGpuInventory" {
		return e.mapReadExec.Execute(ctx, action, args)
	}
	e.calls = append(e.calls, fakeReadExecCall{action: action, args: args})
	if _, pod := args["zone_id"]; pod {
		return e.pod, nil
	}
	return e.official, nil
}

func TestStockHandle_PositiveCapacityUsesGroupedCardCountsAndQueriesBothInventoryBackends(t *testing.T) {
	base := &mapReadExec{results: map[string]map[string]any{
		"DescribeAvailableCompShareInstanceTypes": {"AvailableInstanceTypes": []any{
			map[string]any{"Name": "4090", "Zone": "cn-bj2-03", "Status": "Normal"},
			map[string]any{"Name": "4090", "Zone": "cn-wlcb-03", "Status": "Normal"},
		}},
		"DescribeCompShareSupportZone": stockSupportZonesFixture(),
		"DescribeCompShareImages": {"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-system", "Status": "Available", "ImageType": "System"},
		}},
		"CheckCompShareResourceCapacity": {"Specs": []any{
			map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true},
		}},
	}}
	exec := &stockDualInventoryExec{
		mapReadExec: base,
		official: map[string]any{"GpuInventory": map[string]any{
			"Exclusive": map[string]any{
				"5001":  map[string]any{"4090": float64(0)},
				"10033": map[string]any{"4090": float64(0)},
			},
			"Spot": map[string]any{},
		}},
		pod: map[string]any{"GpuInventory": map[string]any{
			"Exclusive": map[string]any{"5001": map[string]any{"4090": float64(12)}},
			"Spot":      map[string]any{"10033": map[string]any{"4090": float64(7)}},
		}},
	}

	reg := NewReadCapability(stockReadSpec())
	result := reg.Run(context.Background(), StockAvailabilityRequest{
		GPUType: "4090", ZoneMentions: []string{"华北一C", "华北二C"},
	}, ReadRuntime{
		Executor: exec, TopOrganizationID: 66391350, OrganizationID: 64404856,
	})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Contains(t, result.Reply, "- 4090 / 华北一C (cn-bj2-03)：1 卡")
	assert.Contains(t, result.Reply, "- 4090 / 华北二C (cn-wlcb-03)：1 卡")
	assert.NotContains(t, result.Reply, "约 12 张",
		"successful capacity output stays a card-count table; raw pool totals are only validation evidence")
	assert.NotContains(t, result.Reply, "约 7 张")
	assert.Equal(t, ReadPresentationRequired, result.Presentation,
		"the exact capacity table must survive model wording")

	var inventoryCalls []fakeReadExecCall
	for _, call := range exec.calls {
		if call.action == "DescribeCompShareGpuInventory" {
			inventoryCalls = append(inventoryCalls, call)
		}
	}
	require.Len(t, inventoryCalls, 2)
	assert.NotContains(t, inventoryCalls[0].args, "zone_id", "official backend is selected by an empty zone selector")
	assert.NotZero(t, inventoryCalls[1].args["zone_id"], "any live Pod zone id selects the Pod backend")
	for _, call := range exec.calls {
		if call.action == "DescribeCompShareSupportZone" || call.action == "DescribeCompShareGpuInventory" {
			assert.Equal(t, uint32(66391350), call.args["top_organization_id"])
			assert.Equal(t, uint32(64404856), call.args["organization_id"])
		}
	}
}

func TestStockHandle_PodInventoryStillReportedWhenSaleCatalogOmitsRequestedZone(t *testing.T) {
	base := &mapReadExec{results: map[string]map[string]any{
		"DescribeAvailableCompShareInstanceTypes": {"AvailableInstanceTypes": []any{
			map[string]any{"Name": "4090", "Zone": "cn-wlcb-01", "Status": "Normal"},
		}},
		"DescribeCompShareSupportZone": stockSupportZonesFixture(),
	}}
	exec := &stockDualInventoryExec{
		mapReadExec: base,
		official: map[string]any{"GpuInventory": map[string]any{
			"Exclusive": map[string]any{}, "Spot": map[string]any{},
		}},
		pod: map[string]any{"GpuInventory": map[string]any{
			"Exclusive": map[string]any{},
			"Spot":      map[string]any{"10033": map[string]any{"4090": float64(7)}},
		}},
	}

	result := runStock(t, exec, "", StockAvailabilityRequest{
		GPUType: "4090", ZoneMentions: []string{"华北二C"},
	})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Contains(t, result.Reply, "未在指定可用区")
	assert.Contains(t, result.Reply, "华北二C (cn-wlcb-03) / 4090：抢占约 7 张")
	assert.Contains(t, result.Reply, "未执行跨区容量预检")
	for _, call := range exec.calls {
		assert.NotEqual(t, "CheckCompShareResourceCapacity", call.action,
			"a raw inventory row must not invent a machine configuration for capacity precheck")
	}
}

func TestStockHandle_PrechecksEverySaleZone(t *testing.T) {
	exec := &mapReadExec{results: map[string]map[string]any{
		"DescribeAvailableCompShareInstanceTypes": {
			"AvailableInstanceTypes": []any{
				map[string]any{
					"Name": "4090", "Zone": "cn-wlcb-01", "Status": "Normal",
					"Disks": []any{map[string]any{"BootDisk": []any{map[string]any{"Name": "CLOUD_SSD", "MinimalSize": float64(100)}}}},
				},
				map[string]any{
					"Name": "4090", "Zone": "cn-sh2-02", "Status": "Normal",
					"Disks": []any{map[string]any{"BootDisk": []any{map[string]any{"Name": "CLOUD_SSD", "MinimalSize": float64(100)}}}},
				},
			},
		},
		"DescribeCompShareSupportZone": stockSupportZonesFixture(),
		"DescribeCompShareImages": {
			"ImageSet": []any{
				map[string]any{"CompShareImageId": "img-system", "Name": "Base System", "Status": "Available", "ImageType": "System"},
			},
		},
		"DescribeCompShareGpuInventory": {
			"GpuInventory": map[string]any{
				"Exclusive": map[string]any{"2": map[string]any{"4090": float64(30)}},
			},
		},
		"CheckCompShareResourceCapacity": {
			"Specs": []any{
				map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true},
			},
		},
	}}

	result := runStock(t, exec, "", StockAvailabilityRequest{GPUType: "4090"})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	var capacityCalls int
	for _, call := range exec.calls {
		if call.action == "CheckCompShareResourceCapacity" {
			capacityCalls++
		}
	}
	assert.Equal(t, 2, capacityCalls, "each advertised zone must be checked; a first-zone success cannot stop the scan")
	assert.Contains(t, result.Reply, "华北二A")
	assert.Contains(t, result.Reply, "上海二B")
	assert.NotContains(t, result.Reply, "原始 GPU 库存",
		"a successful per-zone card-count table must not be followed by approximate pool totals")
	assert.NotContains(t, result.Reply, "约 30 张")
}

func TestStockHandle_MultipleNamedZonesAreExactAndNeverCrossZone(t *testing.T) {
	exec := &mapReadExec{results: map[string]map[string]any{
		"DescribeAvailableCompShareInstanceTypes": {
			"AvailableInstanceTypes": []any{
				map[string]any{"Name": "4090", "Zone": "cn-bj2-03", "Status": "Normal"},
				map[string]any{"Name": "4090", "Zone": "cn-wlcb-03", "Status": "Normal"},
				map[string]any{"Name": "4090", "Zone": "cn-sh2-02", "Status": "Normal"},
			},
		},
		"DescribeCompShareSupportZone": stockSupportZonesFixture(),
		"DescribeCompShareImages": {"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-system", "Status": "Available", "ImageType": "System"},
		}},
		"CheckCompShareResourceCapacity": {"Specs": []any{
			map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true},
		}},
	}}

	result := runStock(t, exec, "", StockAvailabilityRequest{
		GPUType: "4090", ZoneMentions: []string{"华北一C", "华北二C"},
	})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Contains(t, result.Reply, "华北一C (cn-bj2-03)")
	assert.Contains(t, result.Reply, "华北二C (cn-wlcb-03)")
	assert.NotContains(t, result.Reply, "上海二B", "an unrequested zone must never re-enter the answer")
	var zonesCalled []string
	for _, call := range exec.calls {
		if call.action == "CheckCompShareResourceCapacity" {
			if zone, ok := call.args["Zone"].(string); ok {
				zonesCalled = append(zonesCalled, zone)
				continue
			}
			switch id := call.args["zone_id"].(type) {
			case uint32:
				if id == 5001 {
					zonesCalled = append(zonesCalled, "cn-bj2-03")
				} else if id == 10033 {
					zonesCalled = append(zonesCalled, "cn-wlcb-03")
				}
			}
		}
	}
	assert.ElementsMatch(t, []string{"cn-bj2-03", "cn-wlcb-03"}, zonesCalled)
}

func TestStockHandle_UnresolvedNamedZoneFailsClosed(t *testing.T) {
	exec := &mapReadExec{results: map[string]map[string]any{
		"DescribeAvailableCompShareInstanceTypes": {"AvailableInstanceTypes": []any{
			map[string]any{"Name": "4090", "Zone": "cn-sh2-02", "Status": "Normal"},
		}},
		"DescribeCompShareSupportZone": stockSupportZonesFixture(),
	}}

	result := runStock(t, exec, "", StockAvailabilityRequest{GPUType: "4090", ZoneMentions: []string{"华北九Z"}})
	require.Equal(t, platform.ReadStatusConflict, result.Status)
	for _, call := range exec.calls {
		assert.NotEqual(t, "CheckCompShareResourceCapacity", call.action,
			"an unresolved requested zone must never degrade into an all-zone precheck")
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
	reply := renderStockCapacityReply(checks)
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
	reply := renderStockCapacityReply(checks)
	assert.Contains(t, reply, "开售")
	assert.Contains(t, reply, "样本")
	assert.NotContains(t, reply, "暂时不能新建实例", "an all-false sample must not become a global creation denial")
	assert.NotContains(t, reply, "暂无可创建库存")
	assert.NotContains(t, reply, "售罄")
}

func TestStockInventoryExplicitZeroAndNoPositivePrecheckRemainsUncertain(t *testing.T) {
	catalog := stockZoneCatalogSnapshot(zones.ParseSupportZones(stockSupportZonesFixture()))
	snapshot := deployment.NewGPUInventorySnapshot(catalog, map[string]any{
		"GpuInventory": map[string]any{
			"Exclusive": map[string]any{"2": map[string]any{"4090_48G": float64(0)}},
			"Spot":      map[string]any{"2": map[string]any{"4090_48G": float64(0)}},
		},
	}, true, true, nil, true, false)
	checks := []stockCapacityCheck{{
		Name: "4090_48G", Zone: "上海二B (cn-sh2-02)", CanonicalZone: "cn-sh2-02", CheckedSpec: 2,
	}}

	reply := joinStockReply(renderStockCapacityReply(checks), renderStockGPUInventory(snapshot, []string{"4090_48G"}, nil, zones.ParseSupportZones(stockSupportZonesFixture())))
	assert.Contains(t, reply, "不能据此判断该机型无法创建")
	assert.Contains(t, reply, "库存快照为 0")
	assert.NotContains(t, reply, "当前暂无可用库存")
}

// The capacity block is inserted into the answer VERBATIM (render_ref), so its
// shape is what the user reads. A real 4090 turn passes three zones x eight
// CPU/memory variants: flattened, that was 24 items joined by "、" inside one
// sentence. Group by 可用区 and keep the card counts — and say in the header that
// the CPU/memory dimension was collapsed, because dropping detail silently from
// a verbatim factual block is how a summary becomes a claim.
func TestRenderStockCapacity_GroupsByZoneAndCollapsesCPUMemoryVariants(t *testing.T) {
	specs := func(counts ...string) []capacitySpec {
		out := make([]capacitySpec, 0, len(counts)*2)
		for _, c := range counts {
			out = append(out,
				capacitySpec{GPUCount: c, Label: c + "卡/16C/64G"},
				capacitySpec{GPUCount: c, Label: c + "卡/16C/94G"})
		}
		return out
	}
	checks := []stockCapacityCheck{
		{Name: "4090", Zone: "上海二B (cn-sh2-02)", CanonicalZone: "cn-sh2-02", CheckedSpec: 8, EnoughSpecs: specs("1", "2", "4", "8")},
		{Name: "4090", Zone: "华北一C (cn-bj2-03)", CanonicalZone: "cn-bj2-03", CheckedSpec: 8, EnoughSpecs: specs("1", "2", "4", "8")},
		{Name: "4090", Zone: "华北二A (cn-wlcb-01)", CanonicalZone: "cn-wlcb-01", CheckedSpec: 8, EnoughSpecs: specs("1", "2", "4", "8")},
	}

	reply := renderStockCapacityReply(checks)

	lines := strings.Split(reply, "\n")
	require.Len(t, lines, 4, "one header plus one line per zone, not 24 items in one sentence")
	assert.Contains(t, lines[0], "可以新建实例")
	assert.Contains(t, lines[0], "CPU/内存", "the collapsed dimension must be named, not silently dropped")
	assert.Equal(t, "- 4090 / 上海二B (cn-sh2-02)：1、2、4、8 卡", lines[1])
	assert.Equal(t, "- 4090 / 华北一C (cn-bj2-03)：1、2、4、8 卡", lines[2])
	assert.Equal(t, "- 4090 / 华北二A (cn-wlcb-01)：1、2、4、8 卡", lines[3])
	assert.NotContains(t, reply, "16C", "per-configuration CPU/memory rows belong to the create flow, not this summary")
}

// Card counts sort numerically: text order would print 1、2、4、8 correctly by
// luck and 1、16、2、4 wrongly as soon as a 16-card spec exists.
func TestRenderStockCapacity_CardCountsSortNumerically(t *testing.T) {
	checks := []stockCapacityCheck{{
		Name: "H20", Zone: "华北二A (cn-wlcb-01)", CanonicalZone: "cn-wlcb-01", CheckedSpec: 3,
		EnoughSpecs: []capacitySpec{
			{GPUCount: "16", Label: "16卡/128C/1024G"},
			{GPUCount: "2", Label: "2卡/32C/128G"},
			{GPUCount: "1", Label: "1卡/16C/64G"},
		},
	}}

	assert.Contains(t, renderStockCapacityReply(checks), "：1、2、16 卡")
}

func TestStockInventoryZeroDoesNotOverridePositiveCapacity(t *testing.T) {
	checks := []stockCapacityCheck{{
		Name: "4090", Zone: "上海二B (cn-sh2-02)", CanonicalZone: "cn-sh2-02",
		CheckedSpec: 1, EnoughSpecs: []capacitySpec{{GPUCount: "1", Label: "1卡/16C/64G"}},
	}}

	assert.Contains(t, renderStockCapacityReply(checks), "可以新建实例")
	assert.NotContains(t, renderStockCapacityReply(checks), "暂无可用库存")
}

func TestStockInventoryUnavailableNeverBecomesNoStock(t *testing.T) {
	checks := []stockCapacityCheck{{
		Name: "4090_48G", Zone: "上海二B (cn-sh2-02)", CanonicalZone: "cn-sh2-02", CheckedSpec: 2,
	}}
	assert.Contains(t, renderStockCapacityReply(checks), "不能据此判断该机型无法创建")
}

func TestStockInventoryFailedPrecheckAndExplicitZeroNeverBecomesNoStock(t *testing.T) {
	checks := []stockCapacityCheck{{
		Name: "4090", Zone: "上海二B (cn-sh2-02)", CanonicalZone: "cn-sh2-02", Failed: true,
	}}
	reply := renderStockCapacityReply(checks)
	assert.Contains(t, reply, "预检未完成")
	assert.NotContains(t, reply, "暂无可用库存")
	assert.NotContains(t, reply, "无法创建")
}

func TestRequestedInventoryPoolNeverConfusesSpotAndExclusive(t *testing.T) {
	catalog := stockZoneCatalogSnapshot(zones.ParseSupportZones(stockSupportZonesFixture()))
	snapshot := deployment.NewGPUInventorySnapshot(catalog, nil, false, false, map[string]any{
		"GpuInventory": map[string]any{
			"Exclusive": map[string]any{"5001": map[string]any{"4090": float64(15)}},
			"Spot":      map[string]any{"10033": map[string]any{"4090": float64(1)}},
		},
	}, true, true)

	t.Run("spot-only zone rejects exclusive claim", func(t *testing.T) {
		checks := []stockCapacityCheck{{
			Name: "4090", Zone: "华北二C (cn-wlcb-03)", CanonicalZone: "cn-wlcb-03",
			CheckedSpec: 1, EnoughSpecs: []capacitySpec{{GPUCount: "1", Label: "1卡/14C/64G"}},
		}}
		eligible, reply := requestedInventoryPoolView(snapshot, checks, deployment.GPUInventoryPoolExclusive)
		assert.Empty(t, eligible, "capacity preview must not approve an unsupported purchase mode")
		assert.Empty(t, renderStockCapacityReply(eligible), "an unsupported mode must not emit a blank-model on-sale sentence")
		assert.Contains(t, reply, "当前不支持独占购买方式")
		assert.NotContains(t, reply, "暂无独占库存")
	})

	t.Run("exclusive-only zone rejects spot claim", func(t *testing.T) {
		checks := []stockCapacityCheck{{
			Name: "4090", Zone: "华北一C (cn-bj2-03)", CanonicalZone: "cn-bj2-03",
		}}
		eligible, reply := requestedInventoryPoolView(snapshot, checks, deployment.GPUInventoryPoolSpot)
		assert.Empty(t, eligible)
		assert.Contains(t, reply, "当前不支持抢占式购买方式")
	})
}

func TestUnspecifiedInventoryPoolDoesNotBecomeExclusive(t *testing.T) {
	assert.Equal(t, "", normalizeInventoryPool(stockInventoryPoolUnspecified))

	spec := NewReadCapability(stockReadSpec())
	props := spec.Schema()["properties"].(map[string]any)
	pool := props["inventory_pool"].(map[string]any)
	require.Equal(t, []string{
		stockInventoryPoolUnspecified,
		deployment.GPUInventoryPoolExclusive,
		deployment.GPUInventoryPoolSpot,
	}, pool["enum"])
}

func TestRequestedInventoryPoolZeroIsSnapshotNotGlobalDenial(t *testing.T) {
	catalog := stockZoneCatalogSnapshot(zones.ParseSupportZones(stockSupportZonesFixture()))
	snapshot := deployment.NewGPUInventorySnapshot(catalog, map[string]any{
		"GpuInventory": map[string]any{
			"Exclusive": map[string]any{"2": map[string]any{"4090_48G": float64(0)}},
		},
	}, true, true, nil, false, false)
	checks := []stockCapacityCheck{{
		Name: "4090_48G", Zone: "上海二B (cn-sh2-02)", CanonicalZone: "cn-sh2-02", CheckedSpec: 1,
	}}
	eligible, reply := requestedInventoryPoolView(snapshot, checks, deployment.GPUInventoryPoolExclusive)
	require.Len(t, eligible, 1)
	assert.Contains(t, reply, "支持独占购买方式")
	assert.Contains(t, reply, "库存快照为 0")
	assert.Contains(t, reply, "不等同于无法创建")
	assert.NotContains(t, reply, "暂无独占库存")
}

func TestCapacityPrecheckUsesRequestedPoolChargeType(t *testing.T) {
	entry := stockInstanceTypeEntry{Name: "4090", Zone: "cn-wlcb-01"}
	postpay := capacityPrecheckArgs(entry, "img", nil, nil, nil, deployment.GPUInventoryPoolExclusive)
	spot := capacityPrecheckArgs(entry, "img", nil, nil, nil, deployment.GPUInventoryPoolSpot)
	assert.Equal(t, deployment.ChargeTypePostpay, postpay["ChargeType"])
	assert.Equal(t, deployment.ChargeTypeSpot, spot["ChargeType"])
}
