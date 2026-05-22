package intent

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Pricing capability (PR #3, 2026-05-22).
//
// Two-stage handler: stage 1 reads DescribeAvailableCompShareInstanceTypes
// to (a) drive the GPU-name vocabulary for user-text matching and (b)
// pick a default 1-GPU spec (CPU + Memory + Zone) per model. Stage 2
// invokes GetCompShareInstancePrice once per matched GPU model with
// ChargeType omitted, which returns all billing variants (按量 / 包日 /
// 包月 / Spot) in a single response.
//
// Default-spec choice: 1 GPU + the smallest valid CPU/Memory combo in
// the MachineSizes.Collection slice. Most "X 多少钱一小时" askers want
// the entry-level price, not the largest spec.
//
// Why a capability instead of LLM tool-use: baseline trace for
// "4090 多少钱一小时" shows the LLM doing 3 tool calls (1 Describe +
// 2 different GetPrice args) and ~36s / 33k tokens. The deterministic
// path here is two tool calls + a renderer template = ~10s / ~6k tokens.
// Commercial-critical paths shouldn't depend on LLM tool-selection
// variance.

// noInstanceTypesReply — stage 1 returned no machine inventory at all
// (Describe-side failure or empty platform catalog).
// noPricingReply — stage 2 ran but the per-charge-type extraction yielded
// nothing for any matched GPU (Get-side schema drift or empty price block).
// Distinct strings let support diagnose which stage broke.
const (
	noInstanceTypesReply = "未获取到可售机型数据，请稍后重试。"
	noPricingReply       = "未获取到 GPU 价格数据。"
)

// handlePricingQuery is the entry point. Returns a HandlerResult whose
// reply is a markdown price table; ToolAction is always set to the
// registry tool (GetCompShareInstancePrice) — even on early exits where
// stage 1 fetched nothing — so the trace + handler-action-whitelist
// plumbing stays consistent with sibling capability handlers.
func handlePricingQuery(ctx context.Context, h *DemoHandler, req HandlerRequest) HandlerResult {
	const action = "GetCompShareInstancePrice"

	// Stage 1: list available GPU types (vocabulary + default spec source).
	const describeAction = "DescribeAvailableCompShareInstanceTypes"
	describe, fb := executeCapabilityAction(ctx, h, req.Plan.Intent, describeAction, map[string]any{})
	if fb != nil {
		return *fb
	}
	items := mapSliceAt(describe, "AvailableInstanceTypes")
	if len(items) == 0 {
		result := HandledResult(noInstanceTypesReply)
		result.ToolAction = action
		result.ToolArgs = copyArgs(map[string]any{})
		return result
	}

	matched := matchUserTextToInstanceTypeNames(req.UserText, items, true)
	unavailable := detectKnownUnavailableGPUs(req.UserText)

	if len(matched) == 0 {
		// No actionable GPU in the user's text — fall back to a clarify
		// prompt listing available models so the user can pick one.
		prefix := ""
		if len(unavailable) > 0 {
			prefix = strings.Join(unavailable, "、") + " 当前未在 CompShare 平台提供。"
		}
		result := HandledResult(pricingClarifyReply(items, prefix))
		result.ToolAction = action
		result.ToolArgs = copyArgs(map[string]any{})
		return result
	}

	// Stage 2: fetch price for each matched GPU model with a default spec.
	// (action is declared at the top of the function so early exits also
	// stamp ToolAction for trace consistency.)
	priced := []gpuPriceRow{}
	for _, name := range matched {
		spec := pickDefaultPricingSpec(name, items)
		if spec.Zone == "" || spec.Cpu == 0 || spec.Memory == 0 {
			// Spec extraction failed — skip this GPU instead of sending
			// an invalid price call.
			continue
		}
		// spec.Memory is in GB (Describe Collection[].Memory[] schema), but
		// GetCompShareInstancePrice expects MB. Convert here once at the
		// boundary so the rendered header (which says "GB") and the API
		// argument stay consistent.
		args := map[string]any{
			"Zone":    spec.Zone,
			"GpuType": name,
			"Gpu":     1,
			"Cpu":     spec.Cpu,
			"Memory":  spec.Memory * 1024,
		}
		priceRaw, fbInner := executeCapabilityAction(ctx, h, req.Plan.Intent, action, args)
		if fbInner != nil {
			// Tolerate per-GPU failure: continue with the others. A
			// transient backend hiccup on one model shouldn't blank the
			// whole reply.
			continue
		}
		priced = append(priced, gpuPriceRow{
			Name:    name,
			Zone:    spec.Zone,
			Cpu:     spec.Cpu,
			Memory:  spec.Memory,
			RawData: priceRaw,
		})
	}

	if len(priced) == 0 {
		return FallbackBeforeTool(FallbackValidation)
	}

	reply := renderPricingReply(priced, req.UserText)
	result := HandledResult(reply)
	result.ToolAction = action
	result.ToolArgs = copyArgs(map[string]any{})
	return result
}

// pricingDefaultSpec captures the 1-GPU default we use for price calls.
type pricingDefaultSpec struct {
	Zone   string
	Cpu    int
	Memory int
}

// gpuPriceRow bundles one (name, spec, raw-price-result) tuple for the
// renderer. RawData is the GetCompShareInstancePrice response map.
type gpuPriceRow struct {
	Name    string
	Zone    string
	Cpu     int
	Memory  int
	RawData map[string]any
}

// pickDefaultPricingSpec scans the Describe items for the first entry
// whose Name matches gpuName, then drills MachineSizes.Collection[].Memory
// for the smallest (CPU + Memory) 1-GPU combo. Returns zero-valued spec
// if extraction fails — caller skips.
func pickDefaultPricingSpec(gpuName string, items []any) pricingDefaultSpec {
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if safeString(entry, "Name") != gpuName {
			continue
		}
		zone := safeString(entry, "Zone")
		if zone == "" {
			zone = "cn-wlcb-01" // pricing API documents the format; default to wlcb
		}
		sizes := mapSliceAt(entry, "MachineSizes")
		for _, s := range sizes {
			size, ok := s.(map[string]any)
			if !ok {
				continue
			}
			gpuCount := pricingNumericInt(size["Gpu"])
			if gpuCount != 1 {
				continue
			}
			collection := mapSliceAt(size, "Collection")
			for _, c := range collection {
				combo, ok := c.(map[string]any)
				if !ok {
					continue
				}
				cpu := pricingNumericInt(combo["Cpu"])
				mems := mapSliceAt(combo, "Memory")
				if cpu == 0 || len(mems) == 0 {
					continue
				}
				memory := pricingNumericInt(mems[0])
				if memory == 0 {
					continue
				}
				return pricingDefaultSpec{Zone: zone, Cpu: cpu, Memory: memory}
			}
		}
	}
	return pricingDefaultSpec{}
}

// pricingNumericInt extracts an int from common JSON-numeric encodings
// (float64 from json.Unmarshal, int, json.Number, string-shaped digits).
// Returns 0 on any failure — caller treats that as "spec incomplete".
func pricingNumericInt(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case float32:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case string:
		var n int
		_, err := fmt.Sscanf(t, "%d", &n)
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}

// pricingClarifyReply lists the available GPU names so a "价格多少?"
// (no GPU named) question gets a useful prompt back instead of a flat
// "未识别" fallback.
func pricingClarifyReply(items []any, prefix string) string {
	names := map[string]struct{}{}
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if name := safeString(entry, "Name"); name != "" {
			names[name] = struct{}{}
		}
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	out := "请告诉我您想查的 GPU 型号 — 当前可售机型:" + strings.Join(sorted, " / ")
	if prefix != "" {
		return prefix + " " + out
	}
	return out
}

// renderPricingReply formats the per-GPU price rows. ChargeType-omitted
// API returns nested {InstancePrice: {Postpay/Day/Month/Spot: ...}}
// blocks; we drill conservatively and emit "未提供" when a billing
// variant is missing rather than failing the whole reply.
func renderPricingReply(rows []gpuPriceRow, userText string) string {
	lines := []string{}
	for _, row := range rows {
		// row.Memory is in GB (sourced from Describe Collection[].Memory[],
		// which is GB; see pickDefaultPricingSpec).
		header := fmt.Sprintf("### %s · %s · 1卡 / %dvCPU / %dGB",
			row.Name, row.Zone, row.Cpu, row.Memory)
		lines = append(lines, header)

		// Extract Postpay/Day/Month/Spot price strings if present.
		// Schema observed in CompShare price responses:
		//   {InstancePrice: {Postpay: {Price: x.xx, OriginalPrice: y.yy}, ...}}
		// We accept either nested form or a flat {Postpay: x.xx}.
		bill := pricingBillingTable(row.RawData)
		if len(bill) == 0 {
			lines = append(lines, "  价格数据缺失")
			continue
		}
		// Display in conventional order.
		for _, key := range []string{"Postpay", "Spot", "Day", "Month"} {
			label := pricingLabel(key)
			val, ok := bill[key]
			if !ok {
				continue
			}
			lines = append(lines, fmt.Sprintf("- **%s**: %s", label, val))
		}
	}
	if len(lines) == 0 {
		return noPricingReply
	}
	return strings.Join(lines, "\n")
}

func pricingLabel(chargeType string) string {
	switch chargeType {
	case "Postpay":
		return "按量(¥/小时)"
	case "Spot":
		return "抢占式(¥/小时)"
	case "Day":
		return "包日(¥/天)"
	case "Month":
		return "包月(¥/月)"
	}
	return chargeType
}

// pricingBillingTable best-efforts the price-per-charge-type extract
// from the GetCompShareInstancePrice response. Handles three shapes
// observed in production:
//   Shape 1 (flat):   { Postpay: <num>, Day: <num>, ... }
//   Shape 2 (nested): { InstancePrice: { Postpay: { Price: <num>, OriginalPrice: <num> }, ... } }
//   Shape 3 (array, real production form 2026-05-22):
//     { PriceDetails: [{ ChargeType: "Postpay", Instance: 1.88 }, ...],
//       ListPriceDetails: [{ ChargeType: "Postpay", Instance: 1.98 }, ...] }
//
//     PriceDetails = discounted/actual payable; ListPriceDetails = list price.
//     When discount applied (Price < List), we render "¥discounted (原价 ¥list)";
//     otherwise just the single number. OriginalPriceDetails is a synonym for
//     ListPriceDetails in current API output — treated as fallback if List absent.
//
// Returns "¥X.XX" strings keyed by ChargeType label.
func pricingBillingTable(raw map[string]any) map[string]string {
	out := map[string]string{}
	if raw == nil {
		return out
	}
	// Shape 3 takes precedence — it is the actual production response shape;
	// Shapes 1/2 remain for legacy compat / test fixtures.
	if details, ok := raw["PriceDetails"].([]any); ok && len(details) > 0 {
		listPrices := mapChargeTypeToInstance(raw["ListPriceDetails"])
		if len(listPrices) == 0 {
			listPrices = mapChargeTypeToInstance(raw["OriginalPriceDetails"])
		}
		actualPrices := mapChargeTypeToInstance(details)
		for _, key := range []string{"Postpay", "Spot", "Day", "Month", "Dynamic"} {
			act, hasAct := actualPrices[key]
			if !hasAct {
				continue
			}
			actStr := pricingFormatNumber(act)
			if actStr == "" {
				continue
			}
			if listVal, hasList := listPrices[key]; hasList {
				listStr := pricingFormatNumber(listVal)
				if listStr != "" && listStr != actStr {
					out[key] = fmt.Sprintf("%s (原价 %s)", actStr, listStr)
					continue
				}
			}
			out[key] = actStr
		}
		if len(out) > 0 {
			return out
		}
	}
	// Shape 1: flat keys at top level.
	for _, key := range []string{"Postpay", "Spot", "Day", "Month", "Dynamic"} {
		if val, ok := raw[key]; ok {
			if s := pricingFormatNumber(val); s != "" {
				out[key] = s
			}
		}
	}
	if len(out) > 0 {
		return out
	}
	// Shape 2: nested under InstancePrice.
	nested, ok := raw["InstancePrice"].(map[string]any)
	if !ok {
		return out
	}
	for _, key := range []string{"Postpay", "Spot", "Day", "Month", "Dynamic"} {
		val, ok := nested[key]
		if !ok {
			continue
		}
		// Each variant is either a number or a {Price: number, OriginalPrice: number} struct.
		switch t := val.(type) {
		case map[string]any:
			price := pricingFormatNumber(t["Price"])
			orig := pricingFormatNumber(t["OriginalPrice"])
			if price != "" && orig != "" && price != orig {
				out[key] = fmt.Sprintf("%s (原价 %s)", price, orig)
			} else if price != "" {
				out[key] = price
			} else if orig != "" {
				out[key] = orig
			}
		default:
			if s := pricingFormatNumber(val); s != "" {
				out[key] = s
			}
		}
	}
	return out
}

// mapChargeTypeToInstance pulls a {ChargeType: Instance} flat map out of a
// PriceDetails / ListPriceDetails / OriginalPriceDetails array. Returns
// an empty map (never nil-derefs) when the input is the wrong shape.
func mapChargeTypeToInstance(v any) map[string]any {
	out := map[string]any{}
	arr, ok := v.([]any)
	if !ok {
		return out
	}
	for _, entry := range arr {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		ct, _ := m["ChargeType"].(string)
		if ct == "" {
			continue
		}
		if inst, has := m["Instance"]; has {
			out[ct] = inst
		}
	}
	return out
}

func pricingFormatNumber(v any) string {
	switch t := v.(type) {
	case float64:
		return fmt.Sprintf("¥%.2f", t)
	case float32:
		return fmt.Sprintf("¥%.2f", t)
	case int:
		return fmt.Sprintf("¥%d", t)
	case int64:
		return fmt.Sprintf("¥%d", t)
	case string:
		if strings.TrimSpace(t) == "" {
			return ""
		}
		// If the API hands back a pre-formatted string, prefix ¥ unless
		// it already has a currency marker.
		if strings.HasPrefix(t, "¥") || strings.HasPrefix(t, "$") {
			return t
		}
		return "¥" + t
	}
	return ""
}
