package intent

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/zones"
)

var (
	cfsIDPattern   = regexp.MustCompile(`(?i)cfs-[a-z0-9-]+`)
	uhostIDPattern = regexp.MustCompile(`(?i)u(?:host|h)-[a-z0-9-]+`)
	sizeGBPattern  = regexp.MustCompile(`(?i)(\d+)\s*(?:g|gb|gib|GB)`)
	zoneIDPattern  = regexp.MustCompile(`(?i)cn-[a-z0-9]+-\d+`)
)

func handleRefundEstimate(ctx context.Context, h *DemoHandler, req HandlerRequest) HandlerResult {
	const action = "GetCompShareRefundPrice"
	if len(req.Plan.Slots.TargetRefs) == 0 {
		if id := extractUHostIDFromText(req.UserText); id != "" {
			args := map[string]any{"UHostIds": []string{id}}
			raw, routeFallback := executeRouteAction(ctx, h, req.Plan.Intent, action, args)
			if routeFallback != nil {
				return *routeFallback
			}
			result := HandledResult(renderRefundEstimateReply(raw, nil))
			result.ToolAction = action
			result.ToolArgs = copyArgs(args)
			return result
		}
		if id := strings.TrimSpace(req.FallbackInstanceID); id != "" {
			req.Plan.Slots.TargetRefs = []TargetRef{{
				Type:       TargetRefUHostIDUserInput,
				Value:      id,
				Source:     SourcePriorTurn,
				SourceSpan: id,
			}}
		} else {
			result := HandledResult("请先告诉我要估算哪台实例的退费，例如实例名称或实例 ID。退费估算不会释放实例。")
			result.ToolAction = action
			result.ToolArgs = copyArgs(map[string]any{})
			return result
		}
	}
	instances, ids, fb := resolveResourceTargetSnapshots(req.Plan.Slots.TargetRefs, req.Resolver)
	if fb != nil {
		if len(req.Plan.Slots.TargetRefs) == 1 && req.Plan.Slots.TargetRefs[0].Source == SourcePriorTurn {
			result := HandledResult("未找到刚才选中的实例，可能已被删除或当前账号不可见。请重新指定实例名称或实例 ID 后再估算退费。")
			result.ToolAction = action
			result.ToolArgs = copyArgs(map[string]any{})
			return result
		}
		return *fb
	}
	args := map[string]any{"UHostIds": ids}
	raw, routeFallback := executeRouteAction(ctx, h, req.Plan.Intent, action, args)
	if routeFallback != nil {
		return *routeFallback
	}
	reply := renderRefundEstimateReply(raw, instances)
	result := HandledResult(reply)
	result.ToolAction = action
	result.ToolArgs = copyArgs(args)
	return result
}

func renderRefundEstimateReply(raw map[string]any, instances []entity.InstanceSnapshot) string {
	rows := mapSliceAt(raw, "RefundPriceSet")
	if len(rows) == 0 {
		return "未获取到退费估算结果。这个查询只做估算，不会释放实例。"
	}
	nameByID := map[string]string{}
	for _, inst := range instances {
		if inst.UHostId != "" && inst.Name != "" {
			nameByID[inst.UHostId] = inst.Name
		}
	}
	lines := []string{"退费估算结果（只读估算，不会释放实例）："}
	for _, rowAny := range rows {
		row, ok := rowAny.(map[string]any)
		if !ok {
			continue
		}
		id := stringField(row, "UHostId")
		label := id
		if name := nameByID[id]; name != "" {
			label = fmt.Sprintf("%s（%s）", name, id)
		}
		code, hasCode := numericField(row, "Code")
		if hasCode && code != 0 {
			msg := stringField(row, "Message")
			if msg == "" {
				msg = "上游暂未返回可退金额"
			}
			lines = append(lines, fmt.Sprintf("- %s：暂无法估算，%s。", label, msg))
			continue
		}
		if price, ok := numericField(row, "RefundPrice"); ok {
			lines = append(lines, fmt.Sprintf("- %s：预计可退 ¥%.2f。", label, price))
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s：上游未返回可退金额。", label))
	}
	if len(lines) == 1 {
		return "未获取到有效退费估算结果。这个查询只做估算，不会释放实例。"
	}
	return strings.Join(lines, "\n")
}

func handleCFSInfo(ctx context.Context, h *DemoHandler, req HandlerRequest) HandlerResult {
	switch cfsQueryKind(req.UserText) {
	case "refund":
		return handleCFSRefundEstimate(ctx, h, req)
	case "upgrade_price":
		return handleCFSUpgradePrice(ctx, h, req)
	case "create_price":
		return handleCFSCreatePrice(ctx, h, req)
	}
	const action = "DescribeCFS"
	args := map[string]any{}
	if cfsID := extractCFSIDFromText(req.UserText); cfsID != "" {
		args["CfsId"] = cfsID
	}
	raw, fb := executeRouteAction(ctx, h, req.Plan.Intent, action, args)
	if fb != nil {
		return *fb
	}
	reply := renderCFSInfoReply(raw)
	result := HandledResult(reply)
	result.ToolAction = action
	result.ToolArgs = copyArgs(args)
	return result
}

func handleCFSCreatePrice(ctx context.Context, h *DemoHandler, req HandlerRequest) HandlerResult {
	const action = "GetCompShareCFSPrice"
	size, ok := extractSizeGBFromText(req.UserText)
	if !ok {
		result := HandledResult("请补充要创建的 CFS 容量，单位 GB，例如 50GB。CFS 询价只读，不会创建资源。")
		result.ToolAction = action
		result.ToolArgs = copyArgs(map[string]any{})
		return result
	}
	zone, ok := resolveCFSZoneFromText(ctx, h, req.UserText)
	if !ok || zone.Zone == "" || zone.ZoneID == 0 {
		result := HandledResult("请补充要创建 CFS 的可用区。CFS 当前只支持 Pod/容器可用区，询价只读，不会创建资源。")
		result.ToolAction = action
		result.ToolArgs = copyArgs(map[string]any{"Size": size})
		return result
	}
	if !zone.IsPod {
		result := HandledResult(fmt.Sprintf("CFS 当前只支持 Pod/容器可用区，%s 不是 Pod 区，无法询价或创建 CFS。", zone.Zone))
		result.ToolAction = action
		result.ToolArgs = copyArgs(map[string]any{"Size": size, "Zone": zone.Zone})
		return result
	}
	args := map[string]any{
		"Size":       size,
		"Zone":       zone.Zone,
		"ChargeType": cfsChargeTypeFromText(req.UserText),
		"Quantity":   1,
		"zone_id":    zone.ZoneID,
		"az_group":   zone.RegionID,
	}
	raw, fb := executeRouteActionInternal(ctx, h, req.Plan.Intent, action, args)
	if fb != nil {
		return *fb
	}
	result := HandledResult(renderCFSCreatePriceReply(raw, size, zone.Zone))
	result.ToolAction = action
	result.ToolArgs = copyArgs(args)
	return result
}

func handleCFSUpgradePrice(ctx context.Context, h *DemoHandler, req HandlerRequest) HandlerResult {
	const action = "GetCompShareCFSUpgradePrice"
	cfsID := extractCFSIDFromText(req.UserText)
	if cfsID == "" {
		result := HandledResult("请补充要扩容的 CFS ID。CFS 扩容询价只读，不会直接扩容。")
		result.ToolAction = action
		result.ToolArgs = copyArgs(map[string]any{})
		return result
	}
	size, ok := extractSizeGBFromText(req.UserText)
	if !ok {
		result := HandledResult("请补充 CFS 扩容后的目标容量，单位 GB，例如 200GB。Size 是目标容量，不是新增容量。")
		result.ToolAction = action
		result.ToolArgs = copyArgs(map[string]any{"CfsId": cfsID})
		return result
	}
	zoneID, fb := resolveCFSZoneIDFromDescribe(ctx, h, req, cfsID)
	if fb != nil {
		return *fb
	}
	args := map[string]any{"CfsId": cfsID, "Size": size, "zone_id": zoneID}
	raw, fb := executeRouteActionInternal(ctx, h, req.Plan.Intent, action, args)
	if fb != nil {
		return *fb
	}
	result := HandledResult(renderCFSUpgradePriceReply(raw, cfsID, size))
	result.ToolAction = action
	result.ToolArgs = copyArgs(args)
	return result
}

func handleCFSRefundEstimate(ctx context.Context, h *DemoHandler, req HandlerRequest) HandlerResult {
	const action = "GetCompShareCFSRefundPrice"
	cfsID := extractCFSIDFromText(req.UserText)
	if cfsID == "" {
		result := HandledResult("请补充要估算退费的 CFS ID。这个查询只做估算，不会删除或释放 CFS。")
		result.ToolAction = action
		result.ToolArgs = copyArgs(map[string]any{})
		return result
	}
	zoneID, fb := resolveCFSZoneIDFromDescribe(ctx, h, req, cfsID)
	if fb != nil {
		return *fb
	}
	args := map[string]any{"CFSId": cfsID, "zone_id": zoneID}
	raw, fb := executeRouteActionInternal(ctx, h, req.Plan.Intent, action, args)
	if fb != nil {
		return *fb
	}
	result := HandledResult(renderCFSRefundReply(raw, cfsID))
	result.ToolAction = action
	result.ToolArgs = copyArgs(args)
	return result
}

func renderCFSInfoReply(raw map[string]any) string {
	rows := mapSliceAt(raw, "CFSSet")
	if len(rows) == 0 {
		if id := stringField(raw, "CfsId"); id != "" {
			rows = []any{raw}
		}
	}
	if len(rows) == 0 {
		return "未查询到 CFS 共享文件存储。这个查询是只读操作，不会创建、扩容或删除 CFS。"
	}
	lines := []string{"CFS 共享文件存储（只读查询）："}
	shown := 0
	for _, rowAny := range rows {
		if shown >= 10 {
			break
		}
		row, ok := rowAny.(map[string]any)
		if !ok {
			continue
		}
		id := stringField(row, "CfsId", "CFSId")
		name := stringField(row, "Name")
		if name == "" {
			name = id
		}
		sizeText := ""
		if size, ok := numericField(row, "Size"); ok {
			sizeText = fmt.Sprintf("，容量 %.0fGB", size)
		}
		charge := stringField(row, "ChargeType")
		if charge != "" {
			charge = "，计费 " + charge
		}
		mountStatus := stringField(row, "MountStatus")
		if mountStatus != "" {
			mountStatus = "，挂载状态 " + mountStatus
		}
		lines = append(lines, fmt.Sprintf("- %s（%s）%s%s%s", name, id, sizeText, charge, mountStatus))
		shown++
	}
	if len(rows) > shown {
		lines = append(lines, fmt.Sprintf("仅展示前 %d 个；如需精确查询请提供 CFS ID。", shown))
	}
	lines = append(lines, "创建或扩容 CFS 需要走确认流程；删除/解绑能力不由 agent 暴露。")
	return strings.Join(lines, "\n")
}

func cfsQueryKind(text string) string {
	lower := strings.ToLower(text)
	if strings.Contains(lower, "退费") || strings.Contains(lower, "退款") || strings.Contains(lower, "退订") || strings.Contains(lower, "释放能退") {
		return "refund"
	}
	priceLike := strings.Contains(lower, "多少钱") || strings.Contains(lower, "价格") || strings.Contains(lower, "费用") || strings.Contains(lower, "计费")
	if !priceLike {
		return ""
	}
	if strings.Contains(lower, "扩容") || strings.Contains(lower, "扩到") || strings.Contains(lower, "升到") {
		return "upgrade_price"
	}
	if strings.Contains(lower, "cfs") || strings.Contains(text, "共享文件存储") {
		return "create_price"
	}
	return ""
}

func extractSizeGBFromText(text string) (int, bool) {
	match := sizeGBPattern.FindStringSubmatch(text)
	if len(match) < 2 {
		return 0, false
	}
	var size int
	if _, err := fmt.Sscanf(match[1], "%d", &size); err != nil || size <= 0 {
		return 0, false
	}
	return size, true
}

func resolveCFSZoneFromText(ctx context.Context, h *DemoHandler, text string) (zones.ZoneInfo, bool) {
	if h == nil || h.executor == nil {
		return zones.ZoneInfo{}, false
	}
	list, err := zones.FetchSupportZones(ctx, h.executor, 0, 0)
	if err != nil {
		return zones.ZoneInfo{}, false
	}
	if zone := strings.ToLower(strings.TrimSpace(zoneIDPattern.FindString(text))); zone != "" {
		return findCFSZoneInfo(list, zone)
	}
	if zone, ok := zones.ExactZone(list, text); ok {
		return findCFSZoneInfo(list, zone)
	}
	return zones.ZoneInfo{}, false
}

func findCFSZoneInfo(list []zones.ZoneInfo, zone string) (zones.ZoneInfo, bool) {
	for _, item := range list {
		if strings.EqualFold(item.Zone, zone) {
			return item, true
		}
	}
	return zones.ZoneInfo{}, false
}

func resolveCFSZoneIDFromDescribe(ctx context.Context, h *DemoHandler, req HandlerRequest, cfsID string) (uint32, *HandlerResult) {
	raw, fb := executeRouteAction(ctx, h, req.Plan.Intent, "DescribeCFS", map[string]any{"CfsId": cfsID})
	if fb != nil {
		return 0, fb
	}
	zoneID := cfsZoneIDFromDescribe(raw)
	if zoneID == 0 {
		result := HandledResult(fmt.Sprintf("未获取到 %s 所在可用区，暂不能做 CFS 价格或退费估算。这个查询不会修改资源。", cfsID))
		result.ToolAction = "DescribeCFS"
		result.ToolArgs = copyArgs(map[string]any{"CfsId": cfsID})
		return 0, &result
	}
	return zoneID, nil
}

func cfsZoneIDFromDescribe(raw map[string]any) uint32 {
	if raw == nil {
		return 0
	}
	if id := uint32Field(raw, "ZoneId", "ZoneID", "zone_id"); id != 0 {
		return id
	}
	rows := mapSliceAt(raw, "CFSSet")
	for _, rowAny := range rows {
		row, ok := rowAny.(map[string]any)
		if !ok {
			continue
		}
		if id := uint32Field(row, "ZoneId", "ZoneID", "zone_id"); id != 0 {
			return id
		}
	}
	return 0
}

func cfsChargeTypeFromText(text string) string {
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(text, "包年") || strings.Contains(lower, "year"):
		return "Year"
	case strings.Contains(text, "包日") || strings.Contains(lower, "day"):
		return "Day"
	case strings.Contains(text, "动态") || strings.Contains(lower, "dynamic"):
		return "Dynamic"
	default:
		return "Month"
	}
}

func renderCFSCreatePriceReply(raw map[string]any, size int, zone string) string {
	price := cfsPriceFromDetails(raw)
	if price == "" {
		return fmt.Sprintf("未获取到 %s 创建 %dGB CFS 的价格。CFS 询价是只读操作，不会创建资源。", zone, size)
	}
	return fmt.Sprintf("%s 创建 %dGB CFS 的预估价格：%s。CFS 询价是只读操作，不会创建资源；真正创建需要走确认流程。", zone, size, price)
}

func renderCFSUpgradePriceReply(raw map[string]any, cfsID string, size int) string {
	if price, ok := numericField(raw, "Price"); ok {
		return fmt.Sprintf("%s 扩容到 %dGB 的预估差价：¥%.2f。这个查询只做估算，不会直接扩容。", cfsID, size, price)
	}
	return fmt.Sprintf("未获取到 %s 扩容到 %dGB 的价格。这个查询只做估算，不会直接扩容。", cfsID, size)
}

func renderCFSRefundReply(raw map[string]any, cfsID string) string {
	if price, ok := numericField(raw, "RefundPrice"); ok {
		return fmt.Sprintf("%s 当前预计可退 ¥%.2f。这个查询只做估算，不会删除或释放 CFS。", cfsID, price)
	}
	return fmt.Sprintf("未获取到 %s 的退费估算结果。这个查询只做估算，不会删除或释放 CFS。", cfsID)
}

func cfsPriceFromDetails(raw map[string]any) string {
	details := mapSliceAt(raw, "PriceDetails")
	if len(details) == 0 {
		return ""
	}
	row, ok := details[0].(map[string]any)
	if !ok {
		return ""
	}
	for _, key := range []string{"Disks", "Price", "TotalPrice"} {
		if price, ok := numericField(row, key); ok {
			return fmt.Sprintf("¥%.2f", price)
		}
	}
	return ""
}

func extractCFSIDFromText(text string) string {
	return strings.ToLower(strings.TrimSpace(cfsIDPattern.FindString(text)))
}

func extractUHostIDFromText(text string) string {
	return strings.ToLower(strings.TrimSpace(uhostIDPattern.FindString(text)))
}

func numericField(m map[string]any, key string) (float64, bool) {
	switch v := m[key].(type) {
	case int:
		return float64(v), true
	case int32:
		return float64(v), true
	case int64:
		return float64(v), true
	case uint:
		return float64(v), true
	case uint32:
		return float64(v), true
	case uint64:
		return float64(v), true
	case float32:
		return float64(v), true
	case float64:
		return v, true
	}
	return 0, false
}

func uint32Field(m map[string]any, keys ...string) uint32 {
	for _, key := range keys {
		switch v := m[key].(type) {
		case int:
			if v > 0 {
				return uint32(v)
			}
		case int32:
			if v > 0 {
				return uint32(v)
			}
		case int64:
			if v > 0 {
				return uint32(v)
			}
		case uint:
			if v > 0 {
				return uint32(v)
			}
		case uint32:
			if v > 0 {
				return v
			}
		case uint64:
			if v > 0 {
				return uint32(v)
			}
		case float32:
			if v > 0 {
				return uint32(v)
			}
		case float64:
			if v > 0 {
				return uint32(v)
			}
		}
	}
	return 0
}
