package intent

import (
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
							"Cpu":    float64(16),
							"Memory": []any{float64(64), float64(94)},
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
