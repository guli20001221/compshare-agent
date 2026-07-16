package capability

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/compshare-agent/internal/platform"
	"github.com/compshare-agent/internal/zones"
)

// Pricing read capability (migrated from the legacy intent route, PR #3).
//
// Two-stage handler: stage 1 reads DescribeAvailableCompShareInstanceTypes to
// (a) drive the GPU-name vocabulary for matching and (b) pick a default 1-GPU
// spec per model. Stage 2 invokes the account-price API (it returns both payable
// and list prices). The handler consumes its own typed PricingRequest — never
// intent.Slots — and never re-reads the user's sentence.

const (
	pricingCapabilityLabel = "pricing_query"
	pricingDescribeAction  = "DescribeAvailableCompShareInstanceTypes"
	pricingPriceAction     = "GetCompShareInstanceUserPrice"

	// noInstanceTypesReply — stage 1 returned no machine inventory at all.
	// noPricingReply — stage 2 ran but per-charge-type extraction yielded nothing.
	// Distinct strings let support diagnose which stage broke.
	noInstanceTypesReply = "未获取到可售机型数据，请稍后重试。"
	noPricingReply       = "未获取到 GPU 价格数据。"
)

// PricingRequest is the pricing capability's own request contract.
type PricingRequest struct {
	GPUType  string             `json:"gpu_type"`
	GPUCount int                `json:"gpu_count,omitempty"`
	Kind     platform.PriceKind `json:"price_kind,omitempty"`
}

// MissingFields reports the required-but-absent request fields.
func (r PricingRequest) MissingFields() []platform.MissingField {
	if r.GPUType == "" {
		return []platform.MissingField{platform.Missing("gpu_type")}
	}
	return nil
}

// PricingResponse is the typed result of a successful pricing handle: the priced
// rows the renderer formats. Terminal outcomes (no inventory, clarify, failure,
// no priceable spec) are returned by the handler as a ReadResult, not here.
type PricingResponse struct {
	Rows []gpuPriceRow
}

// pricingReadSpec is the migrated pricing capability. It is registered in the
// read catalog and dispatched through the typed kernel.
func pricingReadSpec() ReadCapabilitySpec[PricingRequest, PricingResponse] {
	return ReadCapabilitySpec[PricingRequest, PricingResponse]{
		Label:       pricingCapabilityLabel,
		Description: "查询指定 GPU 机型的账号价或目录价。",
		Schema: objectSchema(map[string]any{
			"gpu_type":   stringSchema(),
			"gpu_count":  map[string]any{"type": "integer", "minimum": 1},
			"price_kind": enumSchema("account", "catalog"),
		}, []string{"gpu_type"}),
		Handle: pricingHandle,
		Render: pricingRender,
	}
}

func pricingHandle(ctx context.Context, req PricingRequest, rt ReadRuntime) (PricingResponse, ReadResult) {
	// Stage 1: list available GPU types (vocabulary + default spec source).
	describe, err := rt.Executor.Execute(ctx, pricingDescribeAction, map[string]any{})
	if err != nil {
		return PricingResponse{}, ReadFailureAfterTool(pricingDescribeAction, pricingCapabilityLabel, err)
	}
	if describe == nil {
		describe = map[string]any{}
	}
	items := platform.MapSliceAt(describe, "AvailableInstanceTypes")
	if len(items) == 0 {
		r := ReadHandled(noInstanceTypesReply)
		r.ToolAction = pricingPriceAction
		return PricingResponse{}, r
	}

	search := strings.TrimSpace(req.GPUType)
	matched := platform.MatchUserTextToInstanceTypeNames(search, items, true)
	if len(matched) == 0 {
		// No actionable GPU named — clarify with the available models.
		r := ReadClarification(pricingClarifyReply(items, ""))
		r.ToolAction = pricingPriceAction
		return PricingResponse{}, r
	}

	supportZones, _ := zones.FetchSupportZones(ctx, rt.Executor, 0, 0)

	// Stage 2: fetch price for each matched GPU model with a default spec.
	priced := []gpuPriceRow{}
	for _, name := range matched {
		spec := pickDefaultPricingSpec(name, items, req.GPUCount)
		if spec.Zone == "" || spec.Cpu == 0 || spec.Memory == 0 {
			// Spec extraction failed — skip this GPU instead of an invalid call.
			continue
		}
		// spec.Memory is GB (Describe Collection[].Memory[]); price APIs expect MB.
		// Convert once at the boundary so header ("GB") and API arg stay consistent.
		// Pass only backend placement ids resolved from DescribeCompShareSupportZone;
		// a bare non-default Zone can trigger RetCode=230.
		args := pricingPriceArgs(name, spec)
		addPricingPlacementArgs(args, spec.Zone, supportZones)
		priceRaw, errInner := rt.Executor.ExecuteInternal(ctx, pricingPriceAction, args)
		if errInner != nil {
			// Tolerate per-GPU failure: a transient hiccup on one model shouldn't
			// blank the whole reply.
			continue
		}
		priced = append(priced, gpuPriceRow{
			Name:    name,
			Zone:    spec.Zone,
			GPU:     spec.GPU,
			Cpu:     spec.Cpu,
			Memory:  spec.Memory,
			RawData: priceRaw,
			Kind:    pricingKindForRequest(req),
		})
	}

	if len(priced) == 0 {
		return PricingResponse{}, ReadFallbackBeforeTool(platform.ReadFallbackValidation)
	}
	return PricingResponse{Rows: priced}, ReadResult{}
}

func pricingRender(resp PricingResponse) ReadResult {
	r := ReadHandled(renderPricingReply(resp.Rows))
	r.ToolAction = pricingPriceAction
	return r
}

func pricingPriceArgs(name string, spec pricingDefaultSpec) map[string]any {
	memMB := spec.Memory * 1024
	return map[string]any{
		"GpuType": name,
		"GPU":     spec.GPU,
		"CPU":     spec.Cpu,
		"Memory":  memMB,
	}
}

func pricingKindForRequest(req PricingRequest) string {
	if req.Kind == platform.PriceKindCatalog {
		return "标准价/目录价"
	}
	if req.Kind == platform.PriceKindAccount {
		return "当前账号价格（含折扣）"
	}
	return "当前账号价格（含折扣）"
}

func addPricingPlacementArgs(args map[string]any, zone string, supportZones []zones.ZoneInfo) {
	if args == nil || zone == "" {
		return
	}
	for _, z := range supportZones {
		if z.Zone != zone {
			continue
		}
		if z.Region != "" {
			args["Region"] = z.Region
		}
		if z.IsPod {
			args["IsPod"] = true
			args["Zone"] = z.Zone
		}
		if z.ZoneID != 0 {
			args["zone_id"] = z.ZoneID
		}
		if z.RegionID != 0 {
			args["az_group"] = z.RegionID
		}
		return
	}
}

// pricingDefaultSpec captures the requested GPU-count machine size selected from
// the live catalog. GPU defaults to one only when the Agent did not provide a
// structured count.
type pricingDefaultSpec struct {
	Zone   string
	GPU    int
	Cpu    int
	Memory int
}

// gpuPriceRow bundles one (name, spec, raw-price-result) tuple for the renderer.
type gpuPriceRow struct {
	Name    string
	Zone    string
	GPU     int
	Cpu     int
	Memory  int
	RawData map[string]any
	Kind    string
}

// pickDefaultPricingSpec scans the Describe items for the first entry whose Name
// matches gpuName, then drills MachineSizes.Collection[].Memory for the smallest
// (CPU + Memory) combo at the requested GPU count. Returns zero-valued spec if
// extraction fails — caller skips.
func pickDefaultPricingSpec(gpuName string, items []any, requestedCounts ...int) pricingDefaultSpec {
	requestedGPU := 1
	if len(requestedCounts) > 0 && requestedCounts[0] > 0 {
		requestedGPU = requestedCounts[0]
	}
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if platform.SafeString(entry, "Name") != gpuName {
			continue
		}
		zone := platform.SafeString(entry, "Zone")
		if zone == "" {
			zone = "cn-wlcb-01" // pricing API documents the format; default to wlcb
		}
		best := pricingDefaultSpec{}
		sizes := platform.MapSliceAt(entry, "MachineSizes")
		for _, s := range sizes {
			size, ok := s.(map[string]any)
			if !ok {
				continue
			}
			gpuCount := pricingNumericInt(size["Gpu"])
			if gpuCount != requestedGPU {
				continue
			}
			collection := platform.MapSliceAt(size, "Collection")
			for _, c := range collection {
				combo, ok := c.(map[string]any)
				if !ok {
					continue
				}
				cpu := pricingNumericInt(combo["Cpu"])
				mems := platform.MapSliceAt(combo, "Memory")
				if cpu == 0 || len(mems) == 0 {
					continue
				}
				memory := pricingNumericInt(mems[0])
				if memory == 0 {
					continue
				}
				for _, mem := range mems {
					parsed := pricingNumericInt(mem)
					if parsed != 0 && parsed < memory {
						memory = parsed
					}
				}
				candidate := pricingDefaultSpec{Zone: zone, GPU: requestedGPU, Cpu: cpu, Memory: memory}
				if best.Cpu == 0 || candidate.Cpu < best.Cpu ||
					(candidate.Cpu == best.Cpu && candidate.Memory < best.Memory) {
					best = candidate
				}
			}
		}
		return best
	}
	return pricingDefaultSpec{}
}

// pricingNumericInt extracts an int from common JSON-numeric encodings. Returns
// 0 on any failure — caller treats that as "spec incomplete".
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

// pricingClarifyReply lists the available GPU names so a "价格多少?" (no GPU
// named) question gets a useful prompt back instead of a flat fallback.
func pricingClarifyReply(items []any, prefix string) string {
	names := map[string]struct{}{}
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if name := platform.SafeString(entry, "Name"); name != "" {
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

// renderPricingReply formats the per-GPU price rows.
func renderPricingReply(rows []gpuPriceRow) string {
	lines := []string{}
	for _, row := range rows {
		// row.Memory is GB (sourced from Describe Collection[].Memory[]).
		kind := row.Kind
		if kind == "" {
			kind = "标准价/目录价"
		}
		gpuCount := row.GPU
		if gpuCount <= 0 {
			gpuCount = 1
		}
		header := fmt.Sprintf("### %s · %s · %d卡 / %dvCPU / %dGB · %s",
			row.Name, row.Zone, gpuCount, row.Cpu, row.Memory, kind)
		lines = append(lines, header)

		bill := pricingBillingTableForKind(row.RawData, kind)
		if len(bill) == 0 {
			lines = append(lines, "  价格数据缺失")
			continue
		}
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

func pricingBillingTableForKind(raw map[string]any, kind string) map[string]string {
	if pricingKindIsCatalog(kind) {
		return pricingListBillingTable(raw)
	}
	out := map[string]string{}
	if raw == nil {
		return out
	}
	// Shape 3 (production) takes precedence; Shapes 1/2 remain for legacy/tests.
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
			normalizePricingChargeTypes(out)
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
		normalizePricingChargeTypes(out)
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
	normalizePricingChargeTypes(out)
	return out
}

func pricingKindIsCatalog(kind string) bool {
	return strings.Contains(kind, "目录价") || strings.Contains(kind, "标准价")
}

func pricingListBillingTable(raw map[string]any) map[string]string {
	out := map[string]string{}
	if raw == nil {
		return out
	}
	listPrices := mapChargeTypeToInstance(raw["ListPriceDetails"])
	if len(listPrices) == 0 {
		listPrices = mapChargeTypeToInstance(raw["OriginalPriceDetails"])
	}
	for _, key := range []string{"Postpay", "Spot", "Day", "Month", "Dynamic"} {
		if val, ok := listPrices[key]; ok {
			if s := pricingFormatNumber(val); s != "" {
				out[key] = s
			}
		}
	}
	normalizePricingChargeTypes(out)
	return out
}

func normalizePricingChargeTypes(out map[string]string) {
	if _, hasPostpay := out["Postpay"]; hasPostpay {
		return
	}
	if dynamic := out["Dynamic"]; dynamic != "" {
		out["Postpay"] = dynamic
	}
}

// mapChargeTypeToInstance pulls a {ChargeType: Instance} flat map out of a
// PriceDetails / ListPriceDetails / OriginalPriceDetails array.
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
		if strings.HasPrefix(t, "¥") || strings.HasPrefix(t, "$") {
			return t
		}
		return "¥" + t
	}
	return ""
}
