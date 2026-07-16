package workflow

import (
	"fmt"
	"strings"
)

// CFS create size bounds (GB). Hand-maintained platform limits: upstream
// exposes no CFS-limits API, so these are pinned from the CreateCFS spec
// (verified 2026-07-11). The same 50–2048 range is surfaced verbatim in the
// CFS tool descriptions (internal/tools/registry.go) — keep them in sync if the
// platform changes the bounds.
const (
	minCFSSizeGB = 50
	maxCFSSizeGB = 2048
)

func CreateCFSDef() *Definition {
	return &Definition{
		Name:        "CreateCFSWorkflow",
		Description: "查询支持区 -> 查询 CFS 价格 -> 确认创建 -> 创建 CFS -> 回查 CFS",
		Steps: []Step{
			stepQuerySupportZonesForCreateCFS(),
			stepQueryCreateCFSPrice(),
			stepConfirmCreateCFS(),
			stepCreateCFS(),
			stepDescribeCreatedCFS(),
		},
		ResultData: func(wfCtx *Context) map[string]any {
			out := map[string]any{
				"Name":        wfCtx.Params["Name"],
				"Size":        wfCtx.Params["Size"],
				"Zone":        wfCtx.Params["Zone"],
				"ChargeType":  cfsChargeType(wfCtx.Params),
				"created_cfs": wfCtx.Result("创建 CFS"),
			}
			if cfsID := cfsIDFromCreateResult(wfCtx.Result("创建 CFS")); cfsID != "" {
				out["CfsId"] = cfsID
			}
			return out
		},
	}
}

func ResizeCFSDef() *Definition {
	return &Definition{
		Name:        "ResizeCFSWorkflow",
		Description: "查询 CFS -> 查询扩容价格 -> 确认扩容 -> 扩容 CFS",
		Steps: []Step{
			stepQueryCFSForResize(),
			stepQueryResizeCFSPrice(),
			stepConfirmResizeCFS(),
			stepResizeCFS(),
		},
		ResultData: func(wfCtx *Context) map[string]any {
			return map[string]any{
				"CfsId":           wfCtx.Params["CfsId"],
				"current_size_gb": wfCtx.Params["CurrentCFSSize"],
				"target_size_gb":  wfCtx.Params["Size"],
			}
		},
	}
}

func stepQuerySupportZonesForCreateCFS() Step {
	return Step{
		Name: "查询支持区",
		Type: StepToolCall,
		Tool: "DescribeCompShareSupportZone",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			args := map[string]any{}
			addWorkflowIdentityArgs(args, wfCtx.Runtime)
			return args, nil
		},
		CheckResult: func(_ *Context, result map[string]any) (bool, string) {
			if !hasSupportZoneEntries(result) {
				return false, "未获取到支持区列表，无法安全创建 CFS。请稍后重试或到控制台确认可用区。"
			}
			return true, ""
		},
	}
}

func stepQueryCreateCFSPrice() Step {
	return Step{
		Name: "查询 CFS 价格",
		Type: StepToolCall,
		Tool: "GetCompShareCFSPrice",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			if err := normalizeCreateCFSParams(wfCtx); err != nil {
				return nil, err
			}
			args := map[string]any{
				"Name":       wfCtx.Params["Name"],
				"Size":       wfCtx.Params["Size"],
				"Zone":       wfCtx.Params["Zone"],
				"Region":     wfCtx.Params["Region"],
				"zone_id":    wfCtx.Params["CFSZoneId"],
				"az_group":   wfCtx.Params["CFSAzGroup"],
				"ChargeType": cfsChargeType(wfCtx.Params),
				"Quantity":   wfCtx.Params["Quantity"],
			}
			addWorkflowIdentityArgs(args, wfCtx.Runtime)
			return args, nil
		},
	}
}

func stepConfirmCreateCFS() Step {
	return Step{
		Name: "确认创建 CFS",
		Type: StepConfirm,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			priceResult := wfCtx.Result("查询 CFS 价格")
			args := map[string]any{
				"Name":       wfCtx.Params["Name"],
				"Size":       wfCtx.Params["Size"],
				"Zone":       wfCtx.Params["Zone"],
				"Region":     wfCtx.Params["Region"],
				"ChargeType": cfsChargeType(wfCtx.Params),
				"Quantity":   wfCtx.Params["Quantity"],
				"warning":    "将创建新的 CFS 共享文件存储并开始计费；CFS 暂不支持按量付费，删除能力不由 agent 暴露。",
			}
			priceText := cfsCreatePriceText(priceResult)
			if priceText == "" {
				return nil, fmt.Errorf(missingWorkflowPriceMessage)
			}
			args["price"] = priceText
			return args, nil
		},
	}
}

func stepCreateCFS() Step {
	return Step{
		Name: "创建 CFS",
		Type: StepToolCall,
		Tool: "CreateCFS",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			args := map[string]any{
				"Name":       wfCtx.Params["Name"],
				"Size":       wfCtx.Params["Size"],
				"Zone":       wfCtx.Params["Zone"],
				"Region":     wfCtx.Params["Region"],
				"zone_id":    wfCtx.Params["CFSZoneId"],
				"az_group":   wfCtx.Params["CFSAzGroup"],
				"ChargeType": cfsChargeType(wfCtx.Params),
				"Quantity":   wfCtx.Params["Quantity"],
			}
			addWorkflowIdentityArgs(args, wfCtx.Runtime)
			return args, nil
		},
	}
}

func stepDescribeCreatedCFS() Step {
	return Step{
		Name:     "回查 CFS",
		Type:     StepToolCall,
		Tool:     "DescribeCFS",
		Optional: true,
		SkipIf: func(wfCtx *Context) (bool, error) {
			return cfsIDFromCreateResult(wfCtx.Result("创建 CFS")) == "", nil
		},
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			args := map[string]any{
				"CfsId": cfsIDFromCreateResult(wfCtx.Result("创建 CFS")),
			}
			addWorkflowIdentityArgs(args, wfCtx.Runtime)
			return args, nil
		},
	}
}

func stepQueryCFSForResize() Step {
	return Step{
		Name: "查询 CFS",
		Type: StepToolCall,
		Tool: "DescribeCFS",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			cfsID := strings.TrimSpace(paramStr(wfCtx.Params, "CfsId", ""))
			if cfsID == "" {
				cfsID = strings.TrimSpace(paramStr(wfCtx.Params, "CFSId", ""))
			}
			if cfsID == "" {
				return nil, NewMissingSlotError("扩容 CFS 需要指定 CFS ID。", "cfs_id")
			}
			targetSize := paramNum(wfCtx.Params, "Size", 0)
			if targetSize <= 0 {
				return nil, NewMissingSlotError("扩容 CFS 需要指定目标容量（GB）。", "target_size_gb")
			}
			wfCtx.Params["CfsId"] = cfsID
			wfCtx.Params["Size"] = targetSize
			args := map[string]any{"CfsId": cfsID}
			addWorkflowIdentityArgs(args, wfCtx.Runtime)
			return args, nil
		},
		CheckResult: func(wfCtx *Context, result map[string]any) (bool, string) {
			cfs, ok := firstCFS(result)
			if !ok {
				return false, "未找到该 CFS。"
			}
			currentSize := cfsNumber(cfs, "Size")
			if currentSize <= 0 {
				return false, "未能识别当前 CFS 容量，无法安全扩容。"
			}
			targetSize := paramNum(wfCtx.Params, "Size", 0)
			if targetSize <= currentSize {
				return false, fmt.Sprintf("目标容量必须大于当前容量：当前 %.0fGB，目标 %.0fGB。CFS 只支持扩容，不支持缩容或保持不变。", currentSize, targetSize)
			}
			zoneID := cfsNumber(cfs, "ZoneId", "ZoneID")
			if zoneID <= 0 {
				return false, "未获取到 CFS 所在可用区编号，无法安全执行扩容。"
			}
			wfCtx.Params["CurrentCFSSize"] = currentSize
			wfCtx.Params["CFSName"] = cfsString(cfs, "Name")
			wfCtx.Params["CFSZoneId"] = zoneID
			return true, ""
		},
	}
}

func stepQueryResizeCFSPrice() Step {
	return Step{
		Name: "查询 CFS 扩容价格",
		Type: StepToolCall,
		Tool: "GetCompShareCFSUpgradePrice",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			args := map[string]any{
				"CfsId":   wfCtx.Params["CfsId"],
				"Size":    wfCtx.Params["Size"],
				"zone_id": wfCtx.Params["CFSZoneId"],
			}
			addWorkflowIdentityArgs(args, wfCtx.Runtime)
			return args, nil
		},
	}
}

func stepConfirmResizeCFS() Step {
	return Step{
		Name: "确认扩容 CFS",
		Type: StepConfirm,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			args := map[string]any{
				"CfsId":           wfCtx.Params["CfsId"],
				"Name":            wfCtx.Params["CFSName"],
				"current_size_gb": wfCtx.Params["CurrentCFSSize"],
				"target_size_gb":  wfCtx.Params["Size"],
				"warning":         "将把 CFS 扩容到目标容量；Size 是目标容量，不是新增容量。扩容后不能缩小。",
			}
			priceResult := wfCtx.Result("查询 CFS 扩容价格")
			price, err := requiredPriceField(priceResult, "Price")
			if err != nil {
				return nil, err
			}
			args["price_delta"] = price
			if original := firstCFSPrice(priceResult, "OriginalPrice", "ListPrice"); original != nil {
				args["original_price_delta"] = original
			}
			return args, nil
		},
	}
}

func stepResizeCFS() Step {
	return Step{
		Name: "扩容 CFS",
		Type: StepToolCall,
		Tool: "ResizeCFS",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			args := map[string]any{
				"CfsId":   wfCtx.Params["CfsId"],
				"Size":    wfCtx.Params["Size"],
				"zone_id": wfCtx.Params["CFSZoneId"],
			}
			addWorkflowIdentityArgs(args, wfCtx.Runtime)
			return args, nil
		},
	}
}

func normalizeCreateCFSParams(wfCtx *Context) error {
	name := strings.TrimSpace(paramStr(wfCtx.Params, "Name", ""))
	if name == "" {
		return NewMissingSlotError("创建 CFS 需要指定名称。", "name")
	}
	size := paramNum(wfCtx.Params, "Size", 0)
	if size < minCFSSizeGB || size > maxCFSSizeGB {
		return NewMissingSlotError(fmt.Sprintf("CFS 容量需在 %dGB 到 %dGB 之间。", minCFSSizeGB, maxCFSSizeGB), "size_gb")
	}
	zone := strings.TrimSpace(paramStr(wfCtx.Params, "Zone", ""))
	if zone == "" {
		return NewMissingSlotError("创建 CFS 需要指定可用区。", "zone")
	}
	region := strings.TrimSpace(paramStr(wfCtx.Params, "Region", ""))
	if region == "" {
		region = regionFromZone(zone)
	}
	isPod, zoneID, azGroup, resolvedRegion, resolved, err := resolveCreateCFSZone(wfCtx, zone)
	if err != nil {
		return err
	}
	if resolved != "" {
		zone = resolved
	}
	if resolvedRegion != "" {
		region = resolvedRegion
	}
	if region == "" {
		return fmt.Errorf("无法从可用区推导地域，请从支持区列表中选择真实 Pod 区。")
	}
	chargeType := cfsChargeType(wfCtx.Params)
	if strings.EqualFold(chargeType, "Postpay") {
		return fmt.Errorf("CFS 不支持按量付费，请选择 Month、Year、Day 或 Dynamic。")
	}
	quantity := paramNum(wfCtx.Params, "Quantity", 1)
	if quantity <= 0 {
		quantity = 1
	}
	wfCtx.Params["Name"] = name
	wfCtx.Params["Size"] = size
	wfCtx.Params["Zone"] = zone
	wfCtx.Params["Region"] = region
	wfCtx.Params["ZoneIsPod"] = isPod
	wfCtx.Params["CFSZoneId"] = zoneID
	wfCtx.Params["CFSAzGroup"] = azGroup
	wfCtx.Params["ChargeType"] = chargeType
	wfCtx.Params["Quantity"] = quantity
	return nil
}

func resolveCreateCFSZone(wfCtx *Context, requested string) (isPod bool, zoneID uint32, azGroup uint32, region string, zone string, err error) {
	if placement, ok := createCFSSupportZonePlacement(wfCtx.Result("查询支持区"), requested); ok {
		if !placement.isPod {
			return false, 0, 0, "", "", fmt.Errorf("CFS 当前只支持 Pod/容器可用区，%s 不是 Pod 区，不能创建 CFS。", requested)
		}
		if placement.zoneID == 0 {
			return false, 0, 0, "", "", fmt.Errorf("未获取到可用区 %s 的内部编号，无法安全创建 CFS。", placement.zone)
		}
		if placement.azGroup == 0 {
			return false, 0, 0, "", "", fmt.Errorf("未获取到可用区 %s 的内部区域编号，无法安全创建 CFS。", placement.zone)
		}
		return true, placement.zoneID, placement.azGroup, placement.region, placement.zone, nil
	}
	if hasSupportZoneEntries(wfCtx.Result("查询支持区")) {
		return false, 0, 0, "", "", fmt.Errorf("未在支持区中找到可用区 %s。请从上游返回的 Pod/容器可用区中选择。", requested)
	}
	return false, 0, 0, "", "", fmt.Errorf("未获取到支持区列表，无法安全创建 CFS。请稍后重试或到控制台确认可用区。")
}

type createCFSZonePlacement struct {
	zone    string
	region  string
	zoneID  uint32
	azGroup uint32
	isPod   bool
}

func createCFSSupportZonePlacement(result map[string]any, requested string) (createCFSZonePlacement, bool) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return createCFSZonePlacement{}, false
	}
	raw, _ := result["ZoneInfo"].([]any)
	for _, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		entryZone := strings.TrimSpace(stringFieldAny(entry["Zone"]))
		describe := strings.TrimSpace(stringFieldAny(entry["Describe"]))
		if requested != entryZone && requested != describe {
			continue
		}
		placement := createCFSZonePlacement{
			zone:   entryZone,
			region: strings.TrimSpace(stringFieldAny(entry["Region"])),
			isPod:  boolFieldAny(entry["IsPod"]),
		}
		if id, ok := parseUint32Any(entry["ZoneId"]); ok {
			placement.zoneID = id
		}
		if id, ok := parseUint32Any(entry["RegionId"]); ok {
			placement.azGroup = id
		}
		return placement, true
	}
	return createCFSZonePlacement{}, false
}

func hasSupportZoneEntries(result map[string]any) bool {
	raw, _ := result["ZoneInfo"].([]any)
	return len(raw) > 0
}

func boolFieldAny(v any) bool {
	switch typed := v.(type) {
	case bool:
		return typed
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "true", "1", "yes", "y":
			return true
		}
	}
	return false
}

// addWorkflowIdentityArgs forwards server-injected identity (org ids) from the
// context's RuntimeMetadata into an upstream call's args. Identity is not a
// business param, so it is sourced from Runtime, never from Params.
func addWorkflowIdentityArgs(args map[string]any, rt RuntimeMetadata) {
	if rt.TopOrganizationID != 0 {
		args["top_organization_id"] = rt.TopOrganizationID
	}
	if rt.OrganizationID != 0 {
		args["organization_id"] = rt.OrganizationID
	}
}

func cfsChargeType(params map[string]any) string {
	switch strings.ToLower(strings.TrimSpace(paramStr(params, "ChargeType", ""))) {
	case "year", "yearly", "yearpay":
		return "Year"
	case "day", "daypay":
		return "Day"
	case "dynamic":
		return "Dynamic"
	case "month", "monthly", "monthpay", "":
		return "Month"
	default:
		return strings.TrimSpace(paramStr(params, "ChargeType", "Month"))
	}
}

func cfsIDFromCreateResult(result map[string]any) string {
	if result == nil {
		return ""
	}
	for _, key := range []string{"CfsId", "CFSId"} {
		if v, ok := result[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func firstCFS(result map[string]any) (map[string]any, bool) {
	if result == nil {
		return nil, false
	}
	if set, ok := result["CFSSet"].([]any); ok && len(set) > 0 {
		if item, ok := set[0].(map[string]any); ok {
			return item, true
		}
	}
	if id := cfsString(result, "CfsId", "CFSId"); id != "" {
		return result, true
	}
	return nil, false
}

func cfsString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key].(string); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func cfsNumber(m map[string]any, keys ...string) float64 {
	for _, key := range keys {
		switch v := m[key].(type) {
		case int:
			return float64(v)
		case int32:
			return float64(v)
		case int64:
			return float64(v)
		case uint32:
			return float64(v)
		case uint64:
			return float64(v)
		case float32:
			return float64(v)
		case float64:
			return v
		}
	}
	return 0
}

func firstCFSPrice(result map[string]any, keys ...string) any {
	if result == nil {
		return nil
	}
	for _, key := range keys {
		if v, ok := result[key]; ok {
			return v
		}
	}
	for _, listKey := range []string{"PriceDetails", "OriginalPriceDetails", "ListPriceDetails"} {
		if details, ok := result[listKey].([]any); ok && len(details) > 0 {
			return details
		}
	}
	return nil
}

// cfsCreatePriceText formats the CFS create confirm-card price string from a
// GetCompShareCFSPrice response. That response has NO flat Price field — the
// payable price lives in PriceDetails[0].Disks (CFS is a single dimension;
// upstream pod/get_compshare_cfs_price.go fills Disks and leaves Instance nil).
// Returning a formatted string keeps the confirm card from emitting the raw
// PriceDetails array, which the frontend renders as "[object Object]" (same
// class as the instance-create #249 fix). This mirrors the read-only CFS price
// route's cfsPriceFromDetails (internal/intent) — bare "¥X.XX", no period unit,
// since the confirm card already shows ChargeType/Quantity separately.
func cfsCreatePriceText(priceResult map[string]any) string {
	if priceResult == nil {
		return ""
	}
	details, ok := priceResult["PriceDetails"].([]any)
	if !ok || len(details) == 0 {
		return ""
	}
	row, ok := details[0].(map[string]any)
	if !ok {
		return ""
	}
	for _, key := range []string{"Disks", "Price", "TotalPrice"} {
		if n, ok := priceNumber(row[key]); ok {
			return fmt.Sprintf("¥%.2f", n)
		}
	}
	return ""
}
