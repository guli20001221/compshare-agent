package intent

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPricingNumericInt covers the JSON-shape tolerance the caller relies on
// (json.Unmarshal hands back float64 for plain numbers; some upstream paths
// hand strings). Zero is the "spec incomplete" sentinel — caller skips.
func TestPricingNumericInt(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int
	}{
		{"float64", float64(16), 16},
		{"int", int(32), 32},
		{"int64", int64(64), 64},
		{"string_digits", "128", 128},
		{"empty_string", "", 0},
		{"non_numeric_string", "abc", 0},
		{"nil", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, pricingNumericInt(tc.in))
		})
	}
}

// TestPickDefaultPricingSpec_ZeroSpecSkips guards the partial-fail tolerance:
// if a Describe entry exists but its Collection has no usable (CPU, Memory)
// combo, we must return a zero spec so the caller skips this GPU instead of
// firing a price call with Cpu=0 / Memory=0 (which the API rejects).
func TestPickDefaultPricingSpec_ZeroSpecSkips(t *testing.T) {
	// Entry exists for "4090" but Collection is empty.
	items := []any{
		map[string]any{
			"Name": "4090",
			"Zone": "cn-wlcb-01",
			"MachineSizes": []any{
				map[string]any{
					"Gpu":        float64(1),
					"Collection": []any{},
				},
			},
		},
	}
	spec := pickDefaultPricingSpec("4090", items)
	assert.Equal(t, pricingDefaultSpec{}, spec, "empty Collection must yield zero spec so caller skips")
}

// TestPickDefaultPricingSpec_PicksSmallestCombo verifies entry-level default:
// 1-GPU + smallest (CPU, Memory) combo is what most "X 多少钱一小时" askers
// want, not the largest spec.
func TestPickDefaultPricingSpec_PicksSmallestCombo(t *testing.T) {
	items := []any{
		map[string]any{
			"Name": "4090",
			"Zone": "cn-wlcb-01",
			"MachineSizes": []any{
				map[string]any{
					"Gpu": float64(1),
					"Collection": []any{
						map[string]any{
							"Cpu":    float64(32),
							"Memory": []any{float64(128), float64(256)},
						},
						map[string]any{
							"Cpu":    float64(16),
							"Memory": []any{float64(94), float64(64)},
						},
					},
				},
			},
		},
	}
	spec := pickDefaultPricingSpec("4090", items)
	assert.Equal(t, "cn-wlcb-01", spec.Zone)
	assert.Equal(t, 16, spec.Cpu)
	assert.Equal(t, 64, spec.Memory, "must pick smallest memory (64GB), not 94GB")
}

// TestPricingClarifyReply_NoUnavailablePrefix is the "no GPU named in user
// text" path — list available models so the user can pick one. Without
// a prefix, must NOT inject "当前未在 CompShare 平台提供".
func TestPricingClarifyReply_NoUnavailablePrefix(t *testing.T) {
	items := []any{
		map[string]any{"Name": "4090"},
		map[string]any{"Name": "A100"},
	}
	reply := pricingClarifyReply(items, "")
	assert.Contains(t, reply, "请告诉我您想查的 GPU 型号")
	assert.Contains(t, reply, "4090")
	assert.Contains(t, reply, "A100")
	assert.NotContains(t, reply, "未在 CompShare")
}

// TestPricingClarifyReply_UnavailablePrefix verifies the H100/RTX3060
// known-unavailable case puts the warning before the model list.
func TestPricingClarifyReply_UnavailablePrefix(t *testing.T) {
	items := []any{map[string]any{"Name": "4090"}}
	reply := pricingClarifyReply(items, "H100 当前未在 CompShare 平台提供。")
	assert.True(t, strings.HasPrefix(reply, "H100 当前未在 CompShare 平台提供。 "),
		"unavailable prefix must lead the reply, got: %s", reply)
	assert.Contains(t, reply, "4090")
}

// TestPricingBillingTable_FlatShape covers the simpler API response form:
// top-level {Postpay: 1.69, Day: ..., Month: ...}.
func TestPricingBillingTable_FlatShape(t *testing.T) {
	raw := map[string]any{
		"Postpay": float64(1.69),
		"Day":     float64(35.0),
		"Month":   float64(900),
	}
	out := pricingBillingTable(raw)
	assert.Equal(t, "¥1.69", out["Postpay"])
	assert.Equal(t, "¥35.00", out["Day"])
	assert.Equal(t, "¥900.00", out["Month"])
}

// TestPricingBillingTable_NestedShape covers the {InstancePrice: {Postpay:
// {Price, OriginalPrice}}} schema — must surface both numbers when they
// differ ("折后价 / 原价") and just the price when they don't.
func TestPricingBillingTable_NestedShape(t *testing.T) {
	raw := map[string]any{
		"InstancePrice": map[string]any{
			"Postpay": map[string]any{
				"Price":         float64(1.69),
				"OriginalPrice": float64(1.98),
			},
			"Spot": map[string]any{
				"Price":         float64(0.30),
				"OriginalPrice": float64(0.30),
			},
		},
	}
	out := pricingBillingTable(raw)
	assert.Contains(t, out["Postpay"], "¥1.69", "discounted price")
	assert.Contains(t, out["Postpay"], "¥1.98", "original price suffix")
	assert.Equal(t, "¥0.30", out["Spot"], "matched discount/original collapses to single number")
}

// TestPricingBillingTable_ArrayShape covers the actual production response
// form observed via probe on 2026-05-22 against api.compshare.cn:
//
//	{ PriceDetails: [{ChargeType, Instance}, ...],
//	  ListPriceDetails: [{ChargeType, Instance}, ...] }
//
// Previously pricingBillingTable returned 0 keys against this shape, so
// every CLI smoke produced "价格数据缺失". User-facing bug, regression
// guard added here. (Codex review remediation 2026-05-22 follow-up.)
func TestPricingBillingTable_ArrayShape(t *testing.T) {
	raw := map[string]any{
		"PriceDetails": []any{
			map[string]any{"ChargeType": "Postpay", "Instance": float64(1.88)},
			map[string]any{"ChargeType": "Dynamic", "Instance": float64(1.88)},
			map[string]any{"ChargeType": "Day", "Instance": float64(41.48)},
			map[string]any{"ChargeType": "Month", "Instance": float64(1131.40)},
			map[string]any{"ChargeType": "Spot", "Instance": float64(1.31)},
		},
		"ListPriceDetails": []any{
			map[string]any{"ChargeType": "Postpay", "Instance": float64(1.98)},
			map[string]any{"ChargeType": "Day", "Instance": float64(43.66)},
			map[string]any{"ChargeType": "Month", "Instance": float64(1190.95)},
			map[string]any{"ChargeType": "Spot", "Instance": float64(1.38)},
		},
	}
	out := pricingBillingTable(raw)
	// Postpay discounted (1.88) vs list (1.98) — must show both.
	assert.Contains(t, out["Postpay"], "¥1.88")
	assert.Contains(t, out["Postpay"], "¥1.98", "list price suffix when discount differs")
	// Spot discounted (1.31) vs list (1.38) — must show both.
	assert.Contains(t, out["Spot"], "¥1.31")
	assert.Contains(t, out["Spot"], "¥1.38")
	// Day/Month likewise.
	assert.Contains(t, out["Day"], "¥41.48")
	assert.Contains(t, out["Month"], "¥1131.40")
}

// TestPricingBillingTable_ArrayShape_NoDiscount covers the same array
// shape but with PriceDetails == ListPriceDetails (no discount applied).
// Output should collapse to a single number, no "(原价 ¥X)" suffix.
func TestPricingBillingTable_ArrayShape_NoDiscount(t *testing.T) {
	raw := map[string]any{
		"PriceDetails": []any{
			map[string]any{"ChargeType": "Postpay", "Instance": float64(1.69)},
		},
		"ListPriceDetails": []any{
			map[string]any{"ChargeType": "Postpay", "Instance": float64(1.69)},
		},
	}
	out := pricingBillingTable(raw)
	assert.Equal(t, "¥1.69", out["Postpay"],
		"matched list/actual collapses to single number")
}

// TestPricingBillingTable_ArrayShape_OriginalPriceDetailsFallback covers
// the case where ListPriceDetails is missing but OriginalPriceDetails is
// present (API alias). The function must accept OriginalPriceDetails as a
// fallback for the list-price source.
func TestPricingBillingTable_ArrayShape_OriginalPriceDetailsFallback(t *testing.T) {
	raw := map[string]any{
		"PriceDetails": []any{
			map[string]any{"ChargeType": "Postpay", "Instance": float64(1.88)},
		},
		"OriginalPriceDetails": []any{
			map[string]any{"ChargeType": "Postpay", "Instance": float64(1.98)},
		},
	}
	out := pricingBillingTable(raw)
	assert.Contains(t, out["Postpay"], "¥1.88")
	assert.Contains(t, out["Postpay"], "¥1.98", "OriginalPriceDetails fallback when List absent")
}

func TestPricingBillingTable_DynamicFallsBackToPostpay(t *testing.T) {
	raw := map[string]any{
		"PriceDetails": []any{
			map[string]any{"ChargeType": "Dynamic", "Instance": float64(1.88)},
		},
	}
	out := pricingBillingTable(raw)
	assert.Equal(t, "¥1.88", out["Postpay"],
		"Dynamic-only hourly price should render through the normal on-demand/Postpay row")
}

// TestRenderPricingReply_AllRowsBillEmpty exercises the "render succeeded
// but every row has missing price data" branch — must return the
// noPricingReply sentinel rather than an empty string or partial junk.
func TestRenderPricingReply_AllRowsBillEmpty(t *testing.T) {
	rows := []gpuPriceRow{
		{Name: "4090", Zone: "cn-wlcb-01", Cpu: 16, Memory: 64, RawData: map[string]any{}},
	}
	reply := renderPricingReply(rows, "4090 多少钱")
	// Even with empty bill, header is still emitted, so "价格数据缺失" appears.
	assert.Contains(t, reply, "价格数据缺失")
	assert.Contains(t, reply, "4090")
}

// TestRenderPricingReply_HeaderUsesGB locks the P3 fix from PR #151 review:
// the header unit label must say GB, not MB. spec.Memory is sourced from
// Describe Collection[].Memory[] which is GB; the API call (separately)
// converts to MB. Header reflects the human-readable GB.
func TestRenderPricingReply_HeaderUsesGB(t *testing.T) {
	rows := []gpuPriceRow{
		{
			Name: "4090", Zone: "cn-wlcb-01", Cpu: 16, Memory: 64,
			RawData: map[string]any{"Postpay": float64(1.69)},
		},
	}
	reply := renderPricingReply(rows, "")
	assert.Contains(t, reply, "1卡 / 16vCPU / 64GB",
		"header must use GB (Describe units), not MB")
	assert.NotContains(t, reply, "64MB",
		"stale MB label must not reappear (P3 fix from PR #151 review)")
}

// TestHandlePricingQuery_PassesMemoryAsMBToAPI is the args-side regression
// guard for reviewer N1. Describe returns Collection[].Memory in GB; the
// price API expects MB. If the * 1024 conversion at the boundary is ever
// dropped (e.g. by a simplification pass), the header will still render
// "64GB" but the API will be sent Memory=64 (== 64 MB), and prices will
// come back for the wrong spec — silent regression. Lock the conversion
// explicitly by asserting GetCompShareInstancePrice receives the MB-form.
func TestHandlePricingQuery_PassesMemoryAsMBToAPI(t *testing.T) {
	// One Describe entry: 4090 with 1 GPU @ (CPU=16, Memory=64GB).
	// We return the SAME map for both Describe and GetPrice (executor stub
	// is single-result); the test only inspects what the handler sent OUT,
	// so the return shape only matters to keep the renderer from blanking.
	exec := &mockHandlerExecutor{result: map[string]any{
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
		// Flat-shape price block so renderPricingReply succeeds.
		"Postpay": float64(1.69),
	}}
	handler := NewDemoHandler(exec)

	result := handlePricingQuery(context.Background(), handler, HandlerRequest{
		Plan:     Plan{Intent: IntentPricingQuery},
		UserText: "4090 多少钱一小时",
	})

	// Sanity: the handler completed (not a FallbackBeforeTool).
	assert.Equal(t, "GetCompShareInstancePrice", result.ToolAction)

	// Find the GetPrice call (Describe is first, GetPrice is second).
	require.GreaterOrEqual(t, len(exec.calls), 2,
		"expected at least 2 executor calls (Describe + GetPrice)")
	assert.Equal(t, "DescribeAvailableCompShareInstanceTypes", exec.calls[0].action,
		"pricing capability must fetch available specs before asking for price")
	var priceCall *handlerExecCall
	for i := range exec.calls {
		if exec.calls[i].action == "GetCompShareInstancePrice" {
			priceCall = &exec.calls[i]
			break
		}
	}
	require.NotNil(t, priceCall, "GetCompShareInstancePrice never invoked")

	// The crux of N1: Memory must be MB (65536), not GB (64).
	memArg, ok := priceCall.args["Memory"]
	require.True(t, ok, "Memory missing from GetCompShareInstancePrice args")
	assert.Equal(t, 64*1024, memArg,
		"GetCompShareInstancePrice.Memory must be MB (64GB * 1024); "+
			"if this fails, the GB→MB boundary conversion was dropped — "+
			"renderer would still say 64GB but API receives wrong spec")
	assert.Equal(t, 16, priceCall.args["Cpu"], "Cpu must pass through as-is")
	assert.Equal(t, 1, priceCall.args["Gpu"], "Gpu default is 1")
	assert.Equal(t, "cn-wlcb-01", priceCall.args["Zone"])
	assert.Equal(t, "4090", priceCall.args["GpuType"])
}

// TestPricingFormatNumber covers the formatter the bill-table extractor
// uses to build "¥X.XX" strings — guards against silently-empty outputs
// when the API returns int / string / nil.
func TestPricingFormatNumber(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"float", float64(1.69), "¥1.69"},
		{"int", int(100), "¥100"},
		{"string_plain", "2.50", "¥2.50"},
		{"string_with_yen", "¥3.30", "¥3.30"},
		{"empty_string", "", ""},
		{"nil", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, pricingFormatNumber(tc.in))
		})
	}
}
