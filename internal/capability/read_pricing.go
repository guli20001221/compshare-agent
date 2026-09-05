package capability

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/compshare-agent/internal/platform"
	"github.com/compshare-agent/internal/zones"
)

// Two-stage handler: stage 1 reads DescribeAvailableCompShareInstanceTypes to
// (a) drive the GPU-name vocabulary for matching and (b) pick a default 1-GPU
// spec per model. Stage 2 invokes the account-price API (it returns both payable
// and list prices).

const (
	pricingCapabilityLabel = "pricing_query"
	pricingDescribeAction  = "DescribeAvailableCompShareInstanceTypes"
	pricingPriceAction     = "GetCompShareInstanceUserPrice"
	pricingDiskPriceAction = "GetCompShareInstancePrice"

	// noInstanceTypesReply — stage 1 returned no machine inventory at all.
	// noPricingReply — stage 2 ran but per-charge-type extraction yielded nothing.
	// Distinct strings let support diagnose which stage broke.
	noInstanceTypesReply = "未获取到可售机型数据，请稍后重试。"
	noPricingReply       = "未获取到 GPU 价格数据。"
)

// PricingRequest is the pricing capability's own request contract.
type PricingRequest struct {
	GPUType     string             `json:"gpu_type"`
	GPUCount    int                `json:"gpu_count,omitempty"`
	Kind        platform.PriceKind `json:"price_kind,omitempty"`
	Zone        string             `json:"zone,omitempty"`
	ChargeTypes []string           `json:"charge_types,omitempty"`
	Disks       []PricingDisk      `json:"disks,omitempty"`
}

// PricingDisk is one prospective disk included in the upstream quote. Type is
// deliberately a catalog value rather than a locally maintained enum: disk
// availability differs by zone and machine type and is validated against the
// live AvailableInstanceTypes response before the price call.
type PricingDisk struct {
	Role   string `json:"role"`
	Type   string `json:"type"`
	SizeGB int    `json:"size_gb"`
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

// pricingReadSpec registers the typed pricing capability.
func pricingReadSpec() ReadCapabilitySpec[PricingRequest, PricingResponse] {
	return ReadCapabilitySpec[PricingRequest, PricingResponse]{
		Label:       pricingCapabilityLabel,
		Description: "查询拟创建 GPU 配置及可选系统盘/数据盘在各可用区的实时账号净报价或目录价。用于配置报价，不用于核对已有实例当前费用；后者使用 DiagnoseBilling。云存储 Pro 使用 CFS 创建报价能力。报价已包含上游适用的减免，但接口不返回免费额度数值。",
		Params: objectParam(map[string]schemaNode{
			"gpu_type":     stringParam().described("真实 GPU 机型名称；精确名称不会自动替换成显存变体。"),
			"gpu_count":    integerParam(1).described("卡数；省略时默认为 1。"),
			"price_kind":   enumParam(platform.PriceKindValues()...).described("account=当前账号实付价（默认）；catalog=目录价。"),
			"zone":         stringParam().described("可选的精确可用区 ID；省略时查询该机型全部可售区域。"),
			"charge_types": arrayParam(enumParam("Postpay", "Spot", "Day", "Month")).described("所需计费方式；省略时返回上游提供的全部方式。"),
			"disks": arrayParam(objectParam(map[string]schemaNode{
				"role":    enumParam("system", "data").described("system=系统盘；data=普通数据盘。"),
				"type":    stringParam().described("实时机型目录返回的精确磁盘类型，例如 CLOUD_SSD 或 CLOUD_RSSD。"),
				"size_gb": integerParam(1).described("磁盘容量 GB。"),
			}, "role", "type", "size_gb")).described("可选磁盘配置；省略时只查算力价格。"),
		}, "gpu_type"),
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
		r := ReadEmpty(noInstanceTypesReply)
		r.ToolAction = pricingPriceAction
		return PricingResponse{}, r
	}

	search := strings.TrimSpace(req.GPUType)
	matched := platform.MatchUserTextToInstanceTypeNames(search, items, false)
	if len(matched) == 0 {
		// No actionable GPU named — clarify with the available models.
		r := ReadClarification(pricingClarifyReply(items, ""))
		r.ToolAction = pricingPriceAction
		return PricingResponse{}, r
	}

	supportZones, _ := zones.FetchSupportZones(ctx, rt.Executor, 0, 0)

	// Stage 2: inspect every matching offering. Upstream row order is not a
	// preference contract: one zone may expose Spot only while another exposes
	// Postpay/Day/Month for the same GPU name.
	priced := []gpuPriceRow{}
	var quoteErr error
	var failedAction string
	for _, name := range matched {
		for _, spec := range pricingSpecs(name, items, req.GPUCount, req.Zone, req.Disks) {
			args := pricingPriceArgs(name, spec, req.Disks)
			addPricingPlacementArgs(args, spec.Zone, supportZones)
			priceRaw, priceAction, errInner := executePricingQuote(ctx, rt, args, req)
			if errInner != nil {
				if quoteErr == nil {
					quoteErr, failedAction = errInner, priceAction
				}
				continue
			}
			bill := pricingBillingTableForKind(priceRaw, pricingKindForRequest(req))
			if !billingTableMatchesRequestedTypes(bill, req.ChargeTypes) {
				continue
			}
			priced = append(priced, gpuPriceRow{
				Name: name,
				// The zone's display label, not the bare id. supportZones is already
				// fetched above for placement args, and the stock capability renders the
				// same zone as 华北一C (cn-bj2-03) — a price quote that says only
				// cn-bj2-03 makes the user match two names for one place by hand.
				// zones.Label falls back to the id when the catalog fetch failed.
				Zone:        zones.Label(supportZones, spec.Zone),
				GPU:         spec.GPU,
				Cpu:         spec.Cpu,
				Memory:      spec.Memory,
				RawData:     priceRaw,
				Kind:        pricingKindForRequest(req),
				ChargeTypes: append([]string(nil), req.ChargeTypes...),
				Disks:       append([]PricingDisk(nil), req.Disks...),
				ToolAction:  priceAction,
			})
		}
	}

	if len(priced) == 0 {
		if quoteErr != nil {
			return PricingResponse{}, ReadFailureAfterTool(failedAction, pricingCapabilityLabel, quoteErr)
		}
		return PricingResponse{}, ReadFallbackBeforeTool(platform.ReadFallbackValidation)
	}
	return PricingResponse{Rows: priced}, ReadResult{}
}

func pricingRender(resp PricingResponse) ReadResult {
	r := ReadHandled(renderPricingReply(resp.Rows))
	r.ToolAction = pricingPriceAction
	if len(resp.Rows) > 0 && resp.Rows[0].ToolAction != "" {
		r.ToolAction = resp.Rows[0].ToolAction
	}
	return r
}

func executePricingQuote(ctx context.Context, rt ReadRuntime, args map[string]any, req PricingRequest) (map[string]any, string, error) {
	if len(req.Disks) == 0 {
		raw, err := rt.Executor.ExecuteInternal(ctx, pricingPriceAction, args)
		return raw, pricingPriceAction, err
	}
	chargeTypes := append([]string(nil), req.ChargeTypes...)
	if len(chargeTypes) == 0 {
		chargeTypes = []string{"Postpay", "Spot", "Day", "Month"}
	}
	merged := map[string]any{
		"PriceDetails":         []any{},
		"OriginalPriceDetails": []any{},
		"ListPriceDetails":     []any{},
	}
	var firstErr error
	succeeded := 0
	for _, chargeType := range chargeTypes {
		one := make(map[string]any, len(args)+1)
		for key, value := range args {
			one[key] = value
		}
		// The single-charge endpoint's public contract uses Gpu/Cpu while the
		// aggregate user-price endpoint uses GPU/CPU. SafeToolExecutor filters by
		// the selected endpoint schema, so normalize before crossing that boundary.
		one["Gpu"] = one["GPU"]
		one["Cpu"] = one["CPU"]
		delete(one, "GPU")
		delete(one, "CPU")
		one["ChargeType"] = chargeType
		raw, err := rt.Executor.ExecuteInternal(ctx, pricingDiskPriceAction, one)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		succeeded++
		for _, key := range []string{"PriceDetails", "OriginalPriceDetails", "ListPriceDetails"} {
			merged[key] = append(merged[key].([]any), platform.MapSliceAt(raw, key)...)
		}
	}
	if succeeded == 0 {
		if firstErr == nil {
			firstErr = fmt.Errorf("磁盘询价没有返回可用结果")
		}
		return nil, pricingDiskPriceAction, firstErr
	}
	return merged, pricingDiskPriceAction, nil
}

func pricingPriceArgs(name string, spec pricingDefaultSpec, disks []PricingDisk) map[string]any {
	memMB := spec.Memory * 1024
	args := map[string]any{
		"GpuType": name,
		"GPU":     spec.GPU,
		"CPU":     spec.Cpu,
		"Memory":  memMB,
	}
	if len(disks) > 0 {
		quoted := make([]any, 0, len(disks))
		for _, disk := range disks {
			quoted = append(quoted, map[string]any{
				"IsBoot": disk.Role == "system",
				"Type":   disk.Type,
				"Size":   disk.SizeGB,
			})
		}
		args["Disks"] = quoted
	}
	return args
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
	Name        string
	Zone        string
	GPU         int
	Cpu         int
	Memory      int
	RawData     map[string]any
	Kind        string
	ChargeTypes []string
	Disks       []PricingDisk
	ToolAction  string
}

// pricingSpecs returns one deterministic minimum CPU/memory spec for every
// matching zone. Missing zones are rejected instead of being replaced by a
// hard-coded region.
func pricingSpecs(gpuName string, items []any, requestedCount int, zoneFilter string, disks []PricingDisk) []pricingDefaultSpec {
	requestedGPU := 1
	if requestedCount > 0 {
		requestedGPU = requestedCount
	}
	byZone := map[string]pricingDefaultSpec{}
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
			continue
		}
		if strings.TrimSpace(zoneFilter) != "" && !strings.EqualFold(zone, strings.TrimSpace(zoneFilter)) {
			continue
		}
		if !pricingDisksSupported(entry, disks) {
			continue
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
		if best.Cpu == 0 || best.Memory == 0 {
			continue
		}
		if current, ok := byZone[zone]; !ok || best.Cpu < current.Cpu ||
			(best.Cpu == current.Cpu && best.Memory < current.Memory) {
			byZone[zone] = best
		}
	}
	zonesSorted := make([]string, 0, len(byZone))
	for zone := range byZone {
		zonesSorted = append(zonesSorted, zone)
	}
	sort.Strings(zonesSorted)
	out := make([]pricingDefaultSpec, 0, len(zonesSorted))
	for _, zone := range zonesSorted {
		out = append(out, byZone[zone])
	}
	return out
}

func pricingDisksSupported(entry map[string]any, requested []PricingDisk) bool {
	if len(requested) == 0 {
		return true
	}
	for _, want := range requested {
		field := "DataDisk"
		if want.Role == "system" {
			field = "BootDisk"
		}
		matched := false
		for _, groupRaw := range platform.MapSliceAt(entry, "Disks") {
			group, ok := groupRaw.(map[string]any)
			if !ok {
				continue
			}
			for _, diskRaw := range platform.MapSliceAt(group, field) {
				disk, ok := diskRaw.(map[string]any)
				if !ok || platform.SafeString(disk, "Name") != want.Type {
					continue
				}
				min := pricingNumericInt(disk["MinimalSize"])
				max := pricingNumericInt(disk["MaximalSize"])
				if (min == 0 || want.SizeGB >= min) && (max == 0 || want.SizeGB <= max) {
					matched = true
				}
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func billingTableMatchesRequestedTypes(table map[string]string, requested []string) bool {
	if len(requested) == 0 {
		return len(table) > 0
	}
	for _, chargeType := range requested {
		if table[chargeType] != "" {
			return true
		}
	}
	return false
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
	case uint32:
		return int(t)
	case uint64:
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

// renderPricingReply renders one section per DISTINCT quote, not one per zone.
//
// pricingSpecs fans a zone-less request out over every zone that offers the GPU,
// because a zone can differ in price or in which charge types it exposes. Zones
// whose spec and price table are identical are merged into one header; a zone
// that genuinely differs still gets its own section.
func renderPricingReply(rows []gpuPriceRow) string {
	lines := []string{}
	for _, group := range groupPricingRowsByQuote(rows) {
		// row.Memory is GB (sourced from Describe Collection[].Memory[]).
		row := group.row
		gpuCount := row.GPU
		if gpuCount <= 0 {
			gpuCount = 1
		}
		lines = append(lines, fmt.Sprintf("### %s · %s · %d卡 / %dvCPU / %dGB · %s",
			row.Name, strings.Join(group.zones, " / "), gpuCount, row.Cpu, row.Memory, pricingKindLabel(row)))
		lines = append(lines, group.body...)
	}
	if len(lines) == 0 {
		return noPricingReply
	}
	return strings.Join(lines, "\n")
}

func pricingKindLabel(row gpuPriceRow) string {
	if row.Kind == "" {
		return "标准价/目录价"
	}
	return row.Kind
}

// pricingQuoteGroup is one rendered quote plus every zone that quoted it.
type pricingQuoteGroup struct {
	row   gpuPriceRow
	zones []string
	body  []string
}

// groupPricingRowsByQuote merges rows that describe the same offering at the
// same price, keyed on the SPEC plus the rendered body — the body is the thing
// the user compares, so two zones are "the same quote" exactly when it matches.
// First-seen order is preserved so the output stays deterministic.
func groupPricingRowsByQuote(rows []gpuPriceRow) []pricingQuoteGroup {
	groups := make([]pricingQuoteGroup, 0, len(rows))
	index := map[string]int{}
	for _, row := range rows {
		body := pricingRowBodyLines(row)
		key := fmt.Sprintf("%s|%d|%d|%d|%s|%s",
			row.Name, row.GPU, row.Cpu, row.Memory, pricingKindLabel(row), strings.Join(body, "\n"))
		if at, ok := index[key]; ok {
			if !containsExact(groups[at].zones, row.Zone) {
				groups[at].zones = append(groups[at].zones, row.Zone)
			}
			continue
		}
		index[key] = len(groups)
		groups = append(groups, pricingQuoteGroup{row: row, zones: []string{row.Zone}, body: body})
	}
	return groups
}

// pricingRowBodyLines renders one row's price lines — everything under its
// header. Extracted so the grouping above can compare bodies before deciding how
// many headers to print.
func pricingRowBodyLines(row gpuPriceRow) []string {
	kind := pricingKindLabel(row)
	bill := pricingBillingTableForKind(row.RawData, kind)
	if len(bill) == 0 {
		return []string{"  价格数据缺失"}
	}
	lines := []string{}
	for _, key := range []string{"Postpay", "Spot", "Day", "Month"} {
		if len(row.ChargeTypes) > 0 && !containsExact(row.ChargeTypes, key) {
			continue
		}
		label := pricingLabel(key)
		val, ok := bill[key]
		if !ok {
			continue
		}
		if len(row.Disks) == 0 {
			lines = append(lines, fmt.Sprintf("- **%s**: %s", label, val))
			continue
		}
		lines = append(lines, fmt.Sprintf("- **%s**: %s", label, pricingDetailedPrice(row.RawData, kind, key, val, row.Disks)))
	}
	if len(row.Disks) > 0 {
		lines = append(lines, "  磁盘金额是上游返回的当前账号净报价；系统盘免费额度如适用已在上游扣除，但接口不返回额度数值，不能从 ¥0 反推免费额度。")
	}
	return lines
}

func pricingDetailedPrice(raw map[string]any, kind, chargeType, instanceText string, requested []PricingDisk) string {
	rowsKey := "PriceDetails"
	if pricingKindIsCatalog(kind) {
		rowsKey = "ListPriceDetails"
		if len(platform.MapSliceAt(raw, rowsKey)) == 0 {
			rowsKey = "OriginalPriceDetails"
		}
	}
	for _, item := range platform.MapSliceAt(raw, rowsKey) {
		row, ok := item.(map[string]any)
		if !ok || platform.SafeString(row, "ChargeType") != chargeType {
			continue
		}
		parts := []string{"算力 " + instanceText}
		needSystem, needData := pricingDiskRoles(requested)
		systemAmount, hasSystemAmount := numericFloat(row["SystemDisks"])
		diskAmount, hasDiskAmount := numericFloat(row["Disks"])
		if needSystem {
			if hasSystemAmount {
				parts = append(parts, "系统盘 "+pricingFormatNumber(systemAmount))
			} else {
				parts = append(parts, "系统盘金额未返回")
			}
		}
		if needData {
			dataAmount, hasDataAmount := diskAmount, hasDiskAmount
			if hasDataAmount && hasSystemAmount {
				dataAmount -= systemAmount
				hasDataAmount = dataAmount >= 0
			} else if needSystem {
				hasDataAmount = false
			}
			if hasDataAmount {
				parts = append(parts, "数据盘合计 "+pricingFormatNumber(dataAmount))
			} else {
				parts = append(parts, "数据盘金额未返回")
			}
		}
		if value := pricingFormatNumber(row["CompShareImage"]); value != "" {
			parts = append(parts, "付费镜像 "+value)
		}
		if total, ok := pricingTotal(row, requested); ok {
			parts = append(parts, fmt.Sprintf("合计 ¥%.2f", total))
		}
		return strings.Join(parts, "；")
	}
	return "上游未返回分项价格"
}

func pricingTotal(row map[string]any, requested []PricingDisk) (float64, bool) {
	instance, ok := numericFloat(row["Instance"])
	if !ok {
		return 0, false
	}
	total := instance
	needSystem, needData := pricingDiskRoles(requested)
	if value, present := numericFloat(row["Disks"]); present {
		total += value
	} else if needData {
		return 0, false
	} else if value, present := numericFloat(row["SystemDisks"]); present {
		total += value
	} else if needSystem {
		return 0, false
	}
	if value, present := numericFloat(row["CompShareImage"]); present {
		total += value
	}
	return total, true
}

func pricingDiskRoles(disks []PricingDisk) (system, data bool) {
	for _, disk := range disks {
		system = system || disk.Role == "system"
		data = data || disk.Role == "data"
	}
	return system, data
}

func containsExact(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
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
