package capability

import (
	"context"
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

// fakeReadExec is a single-result executor stub mirroring the legacy
// mockHandlerExecutor: it records every call and returns the same map for
// Describe, GetPrice and DescribeCompShareSupportZone. Support-zone parsing
// yields no zones from the pricing fixture, so no placement args are added —
// exactly the legacy behaviour the parity assertions below lock in.
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
	assert.Empty(t, pricingSpecs("4090", items, 1, ""), "missing placement must not become a hard-coded zone")
}
