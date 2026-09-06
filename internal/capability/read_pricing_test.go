package capability

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/compshare-agent/internal/platform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeReadExecCall struct {
	action string
	args   map[string]any
}

// fakeReadExec records calls and returns one shared fixture. Pricing fixtures
// carry both the machine and zone catalogs alongside their price response.
type fakeReadExec struct {
	result map[string]any
	calls  []fakeReadExecCall
}

type pricingByZoneExec struct {
	catalog map[string]any
	prices  map[int]map[string]any
	calls   []fakeReadExecCall
}

func (f *pricingByZoneExec) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	f.calls = append(f.calls, fakeReadExecCall{action: action, args: args})
	switch action {
	case pricingDescribeAction:
		return f.catalog, nil
	case "DescribeCompShareSupportZone":
		return map[string]any{"ZoneInfo": []any{
			map[string]any{"Zone": "cn-wlcb-01", "ZoneId": float64(1)},
			map[string]any{"Zone": "cn-wlcb-03", "ZoneId": float64(3)},
		}}, nil
	case pricingPriceAction:
		return f.prices[pricingNumericInt(args["zone_id"])], nil
	default:
		return map[string]any{}, nil
	}
}

func (f *pricingByZoneExec) ExecuteInternal(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	return f.Execute(ctx, action, args)
}

func (f *fakeReadExec) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	f.calls = append(f.calls, fakeReadExecCall{action: action, args: args})
	return f.result, nil
}

func (f *fakeReadExec) ExecuteInternal(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	return f.Execute(ctx, action, args)
}

func runPricing(t *testing.T, exec ReadExecutor, req PricingRequest) ReadResult {
	t.Helper()
	reg := NewReadCapability(pricingReadSpec())
	return reg.Run(context.Background(), req, ReadRuntime{Executor: exec})
}

func pricingFixture(extra map[string]any) map[string]any {
	result := map[string]any{
		"ZoneInfo": []any{map[string]any{"Zone": "cn-wlcb-01", "ZoneId": float64(1), "Region": "cn-wlcb"}},
		"AvailableInstanceTypes": []any{
			map[string]any{
				"Name": "4090",
				"Zone": "cn-wlcb-01",
				"MachineSizes": []any{
					map[string]any{
						"Gpu": float64(1),
						"Collection": []any{
							map[string]any{
								"Cpu":    float64(16),
								"Memory": []any{float64(64)},
							},
						},
					},
				},
				"Disks": []any{map[string]any{
					"BootDisk": []any{map[string]any{"Name": "CLOUD_SSD", "MinimalSize": float64(40), "MaximalSize": float64(500)}},
					"DataDisk": []any{map[string]any{"Name": "CLOUD_SSD", "MinimalSize": float64(10), "MaximalSize": float64(8000)}},
				}},
			},
		},
	}
	maps.Copy(result, extra)
	return result
}

// TestPricingRequestMissingFields locks the validation contract: an empty
// gpu_type is a structured missing field, not a Chinese substring.
func TestPricingRequestMissingFields(t *testing.T) {
	require.Equal(t, []platform.MissingField{{Name: "gpu_type", Reason: "required"}}, PricingRequest{}.MissingFields())
	require.Nil(t, PricingRequest{GPUType: "4090"}.MissingFields())
}

func TestPricingUpstreamFailureIsNotAParameterCorrection(t *testing.T) {
	exec := &mapReadExec{
		results: map[string]map[string]any{
			pricingDescribeAction:          pricingFixture(nil),
			"DescribeCompShareSupportZone": pricingFixture(nil),
		},
		errs: map[string]error{pricingPriceAction: errors.New("upstream temporarily unavailable")},
	}
	result := runPricing(t, exec, PricingRequest{GPUType: "4090"})
	require.Equal(t, platform.ReadStatusFailureAfterTool, result.Status)
	assert.Equal(t, pricingPriceAction, result.ToolAction)
	assert.Empty(t, result.FallbackReason)
}

func TestPricingUsesRequestedZoneOrReportsCatalogFailure(t *testing.T) {
	catalog := pricingFixture(nil)
	catalog["AvailableInstanceTypes"].([]any)[0].(map[string]any)["Zone"] = "cn-bj2-03"
	for _, unavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "zone_resolved", true: "catalog_unavailable"}[unavailable], func(t *testing.T) {
			exec := &mapReadExec{results: map[string]map[string]any{
				pricingDescribeAction: catalog,
				"DescribeCompShareSupportZone": {"ZoneInfo": []any{map[string]any{
					"Zone": "cn-bj2-03", "ZoneId": float64(5001), "Region": "cn-bj2", "IsPod": true,
				}}},
				pricingPriceAction: {"PriceDetails": []any{map[string]any{"ChargeType": "Postpay", "Instance": float64(1.23)}}},
			}}
			if unavailable {
				exec.errs = map[string]error{"DescribeCompShareSupportZone": errors.New("temporary catalog outage")}
			}
			result := runPricing(t, exec, PricingRequest{GPUType: "4090", Zone: "cn-bj2-03"})
			if unavailable {
				require.Equal(t, platform.ReadStatusFailureAfterTool, result.Status)
				assert.Equal(t, "DescribeCompShareSupportZone", result.ToolAction)
				require.Len(t, exec.calls, 2, "a quote without the requested location must not be dispatched")
				return
			}
			require.Equal(t, platform.ReadStatusHandled, result.Status)
			require.Len(t, exec.calls, 3)
			assert.Equal(t, "cn-bj2", exec.calls[2].args["Region"])
			assert.Equal(t, "cn-bj2-03", exec.calls[2].args["Zone"])
			assert.Equal(t, uint32(5001), exec.calls[2].args["zone_id"])
			assert.Equal(t, true, exec.calls[2].args["IsPod"])
			assert.Contains(t, result.Reply, "cn-bj2-03")
			assert.Contains(t, result.Reply, "¥1.23")
		})
	}
}

// TestPricingHandle_PassesMemoryAsMBToAPI is the typed-path parity twin of the
// legacy TestHandlePricingQuery_PassesMemoryAsMBToAPI: the GB→MB boundary
// conversion, the omitted Zone (RetCode=230 guard) and the 1-GPU default must
// survive the migration off intent.Slots.
func TestPricingHandle_PassesMemoryAsMBToAPI(t *testing.T) {
	exec := &fakeReadExec{result: pricingFixture(map[string]any{"Postpay": float64(1.69)})}

	result := runPricing(t, exec, PricingRequest{GPUType: "4090", Kind: platform.PriceKindAccount})

	assert.Equal(t, "GetCompShareInstanceUserPrice", result.ToolAction)
	assert.Equal(t, platform.ReadStatusHandled, result.Status)
	require.GreaterOrEqual(t, len(exec.calls), 2, "expected Describe + GetPrice")
	assert.Equal(t, "DescribeAvailableCompShareInstanceTypes", exec.calls[0].action)

	var priceCall *fakeReadExecCall
	for i := range exec.calls {
		if exec.calls[i].action == "GetCompShareInstanceUserPrice" {
			priceCall = &exec.calls[i]
			break
		}
	}
	require.NotNil(t, priceCall, "GetCompShareInstanceUserPrice never invoked")
	assert.Equal(t, 64*1024, priceCall.args["Memory"], "Memory must be MB (64GB*1024), not GB")
	assert.Equal(t, 16, priceCall.args["CPU"])
	assert.Equal(t, 1, priceCall.args["GPU"])
	assert.Equal(t, "4090", priceCall.args["GpuType"])
	_, hasZone := priceCall.args["Zone"]
	assert.False(t, hasZone, "Zone must be omitted from the price call (RetCode=230 guard)")
}

func TestPricingHandle_QuotesSupportedDiskComponentsWithoutInventingQuota(t *testing.T) {
	exec := &fakeReadExec{result: pricingFixture(map[string]any{
		"PriceDetails": []any{map[string]any{
			"ChargeType": "Postpay", "Instance": float64(1.58),
			"SystemDisks": float64(0.10), "Disks": float64(0.24),
		}},
	})}

	result := runPricing(t, exec, PricingRequest{
		GPUType: "4090", ChargeTypes: []string{"Postpay"},
		Disks: []PricingDisk{
			{Role: "system", Type: "CLOUD_SSD", SizeGB: 100},
			{Role: "data", Type: "CLOUD_SSD", SizeGB: 200},
		},
	})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Contains(t, result.Reply, "算力 ¥1.58")
	assert.Contains(t, result.Reply, "系统盘 ¥0.10")
	assert.Contains(t, result.Reply, "数据盘合计 ¥0.14")
	assert.NotContains(t, result.Reply, "数据盘合计 ¥0.24")
	assert.Contains(t, result.Reply, "合计 ¥1.82")
	assert.Contains(t, result.Reply, "不能从 ¥0 反推免费额度")
	var priceCall *fakeReadExecCall
	for i := range exec.calls {
		if exec.calls[i].action == pricingDiskPriceAction {
			priceCall = &exec.calls[i]
		}
	}
	require.NotNil(t, priceCall)
	assert.Equal(t, "Postpay", priceCall.args["ChargeType"])
	assert.Equal(t, 1, priceCall.args["Gpu"])
	assert.Equal(t, 16, priceCall.args["Cpu"])
	assert.NotContains(t, priceCall.args, "GPU")
	assert.NotContains(t, priceCall.args, "CPU")
	assert.Equal(t, []any{
		map[string]any{"IsBoot": true, "Type": "CLOUD_SSD", "Size": 100},
		map[string]any{"IsBoot": false, "Type": "CLOUD_SSD", "Size": 200},
	}, priceCall.args["Disks"])
}

func TestPricingHandle_SystemDiskIsNotCountedOrRenderedAsData(t *testing.T) {
	exec := &fakeReadExec{result: pricingFixture(map[string]any{
		"PriceDetails": []any{map[string]any{
			"ChargeType": "Postpay", "Instance": float64(1.58),
			"SystemDisks": float64(0.10), "Disks": float64(0.10),
		}},
	})}

	result := runPricing(t, exec, PricingRequest{
		GPUType: "4090", ChargeTypes: []string{"Postpay"},
		Disks: []PricingDisk{{Role: "system", Type: "CLOUD_SSD", SizeGB: 100}},
	})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Contains(t, result.Reply, "系统盘 ¥0.10")
	assert.NotContains(t, result.Reply, "数据盘")
	assert.Contains(t, result.Reply, "合计 ¥1.68")
}

func TestPricingHandle_RejectsDiskTypeOrSizeAbsentFromZoneCatalog(t *testing.T) {
	exec := &fakeReadExec{result: pricingFixture(map[string]any{
		"PriceDetails": []any{map[string]any{"ChargeType": "Postpay", "Instance": float64(1.58)}},
	})}

	result := runPricing(t, exec, PricingRequest{
		GPUType: "4090",
		Disks:   []PricingDisk{{Role: "data", Type: "CLOUD_RSSD", SizeGB: 200}},
	})

	assert.Equal(t, platform.ReadStatusFallbackBeforeTool, result.Status)
	for _, call := range exec.calls {
		assert.NotEqual(t, pricingDiskPriceAction, call.action, "unsupported disk must not reach the price API")
	}
}

func TestPricingHandle_DiskQuoteUsesSingleChargePriceAPI(t *testing.T) {
	exec := &fakeReadExec{result: pricingFixture(map[string]any{
		"PriceDetails": []any{map[string]any{"ChargeType": "Postpay", "Instance": float64(2.05), "Disks": float64(0.10)}},
	})}

	result := runPricing(t, exec, PricingRequest{
		GPUType: "4090", ChargeTypes: []string{"Postpay"},
		Disks: []PricingDisk{{Role: "data", Type: "CLOUD_SSD", SizeGB: 200}},
	})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Equal(t, pricingDiskPriceAction, result.ToolAction)
	var diskCalls, aggregateCalls int
	for _, call := range exec.calls {
		if call.action == pricingDiskPriceAction {
			diskCalls++
		}
		if call.action == pricingPriceAction {
			aggregateCalls++
		}
	}
	assert.Equal(t, 1, diskCalls)
	assert.Zero(t, aggregateCalls)
}

func TestPricingHandle_DoesNotPretendMissingDiskPriceWasReturned(t *testing.T) {
	exec := &fakeReadExec{result: pricingFixture(map[string]any{
		"PriceDetails": []any{map[string]any{"ChargeType": "Postpay", "Instance": float64(2.05)}},
	})}

	result := runPricing(t, exec, PricingRequest{
		GPUType: "4090",
		Disks: []PricingDisk{
			{Role: "system", Type: "CLOUD_SSD", SizeGB: 100},
			{Role: "data", Type: "CLOUD_SSD", SizeGB: 200},
		},
	})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Contains(t, result.Reply, "系统盘金额未返回")
	assert.Contains(t, result.Reply, "数据盘金额未返回")
	assert.NotContains(t, result.Reply, "系统盘 ¥0.00")
}

// TestPricingHandle_UserPriceReply mirrors TestHandlePricingQuery_UserPriceUses
// UserPriceTool: the account-price PriceDetails shape renders the discounted
// price under the account-price header.
func TestPricingHandle_UserPriceReply(t *testing.T) {
	exec := &fakeReadExec{result: pricingFixture(map[string]any{
		"PriceDetails":         []any{map[string]any{"ChargeType": "Postpay", "Instance": float64(1.58)}},
		"ListPriceDetails":     []any{map[string]any{"ChargeType": "Postpay", "Instance": float64(2.10)}},
		"OriginalPriceDetails": []any{map[string]any{"ChargeType": "Postpay", "Instance": float64(1.98)}},
	})}

	result := runPricing(t, exec, PricingRequest{GPUType: "4090", Kind: platform.PriceKindAccount})

	assert.Equal(t, "GetCompShareInstanceUserPrice", result.ToolAction)
	assert.Contains(t, result.Reply, "当前账号价格")
	assert.Contains(t, result.Reply, "¥1.58")
}

// TestPricingHandle_UnmatchedGPUClarifies proves the clarify branch: a named but
// unavailable GPU yields a structured clarification listing the real models,
// not a fabricated price.
func TestPricingHandle_UnmatchedGPUClarifies(t *testing.T) {
	exec := &fakeReadExec{result: pricingFixture(nil)}

	result := runPricing(t, exec, PricingRequest{GPUType: "H999"})

	assert.True(t, result.NeedsClarification)
	assert.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Contains(t, result.Reply, "请告诉我您想查的 GPU 型号")
	assert.Contains(t, result.Reply, "4090")
}

// TestPricingHandle_NoInventory covers the stage-1-empty terminal.
func TestPricingHandle_NoInventory(t *testing.T) {
	exec := &fakeReadExec{result: map[string]any{"AvailableInstanceTypes": []any{}}}

	result := runPricing(t, exec, PricingRequest{GPUType: "4090"})

	assert.Equal(t, platform.ReadStatusEmpty, result.Status, "an empty inventory is a structured Empty read")
	assert.Equal(t, noInstanceTypesReply, result.Reply)
}

func TestPricingHandle_DoesNotLetFirstSpotOnlyZoneHideNormalBilling(t *testing.T) {
	entry := func(zone string) map[string]any {
		return map[string]any{
			"Name": "4090", "Zone": zone,
			"MachineSizes": []any{map[string]any{
				"Gpu":        float64(1),
				"Collection": []any{map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}}},
			}},
		}
	}
	exec := &pricingByZoneExec{
		catalog: map[string]any{"AvailableInstanceTypes": []any{entry("cn-wlcb-03"), entry("cn-wlcb-01")}},
		prices: map[int]map[string]any{
			3: {"PriceDetails": []any{map[string]any{"ChargeType": "Spot", "Instance": float64(1.37)}}},
			1: {"PriceDetails": []any{
				map[string]any{"ChargeType": "Postpay", "Instance": float64(1.69)},
				map[string]any{"ChargeType": "Day", "Instance": float64(37)},
				map[string]any{"ChargeType": "Month", "Instance": float64(999)},
			}},
		},
	}

	result := runPricing(t, exec, PricingRequest{
		GPUType: "4090", ChargeTypes: []string{"Postpay", "Day", "Month"},
	})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Contains(t, result.Reply, "cn-wlcb-01")
	assert.Contains(t, result.Reply, "按量")
	assert.Contains(t, result.Reply, "包日")
	assert.Contains(t, result.Reply, "包月")
	assert.NotContains(t, result.Reply, "cn-wlcb-03")
	assert.NotContains(t, result.Reply, "抢占式")
}

func TestPricingHandle_ExactModelDoesNotExpandMemoryVariant(t *testing.T) {
	base := pricingFixture(nil)["AvailableInstanceTypes"].([]any)[0]
	exec := &fakeReadExec{result: map[string]any{
		"ZoneInfo": pricingFixture(nil)["ZoneInfo"],
		"AvailableInstanceTypes": []any{
			base,
			map[string]any{
				"Name": "4090_48G", "Zone": "cn-wlcb-01",
				"MachineSizes": []any{map[string]any{
					"Gpu":        float64(1),
					"Collection": []any{map[string]any{"Cpu": float64(16), "Memory": []any{float64(96)}}},
				}},
			},
		},
		"Postpay": float64(1.69),
	}}

	result := runPricing(t, exec, PricingRequest{GPUType: "4090"})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Contains(t, result.Reply, "### 4090 ·")
	assert.NotContains(t, result.Reply, "4090_48G")
	var priceCalls int
	for _, call := range exec.calls {
		if call.action == pricingPriceAction {
			priceCalls++
		}
	}
	assert.Equal(t, 1, priceCalls)
}

func TestPricingSpecsDoesNotInventZoneWhenCatalogOmitsIt(t *testing.T) {
	items := []any{map[string]any{
		"Name": "4090",
		"MachineSizes": []any{map[string]any{
			"Gpu":        float64(1),
			"Collection": []any{map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}}},
		}},
	}}
	assert.Empty(t, pricingSpecs("4090", items, 1, "", nil), "missing placement must not become a hard-coded zone")
}
