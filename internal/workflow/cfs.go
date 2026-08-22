package workflow

import (
	"fmt"
	"strings"
)

// CFS create size bounds (GB). Upstream exposes no limits API, so these values
// are pinned to the CreateCFS contract and mirrored in the tool descriptions.
const (
	minCFSSizeGB = 50
	maxCFSSizeGB = 2048
)

func CreateCFSDef() *Definition {
	return &Definition{
		Name: "CreateCFSWorkflow",
		Steps: []Step{
			stepQueryExistingCFSForCreate(),
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
				"ChargeType":  cfsChargeTypeLabel(cfsChargeType(wfCtx.Params)),
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
		Name: "ResizeCFSWorkflow",
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

func stepQueryExistingCFSForCreate() Step {
	return Step{
		Name: "检查同区 CFS",
		Type: StepToolCall,
		Tool: "DescribeCFS",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			if err := normalizeCreateCFSParams(wfCtx); err != nil {
				return nil, err
			}
			args := map[string]any{
				"Zone":     wfCtx.Params["Zone"],
				"Region":   wfCtx.Params["Region"],
				"zone_id":  wfCtx.Params["CFSZoneId"],
				"az_group": wfCtx.Params["CFSAzGroup"],
			}
			addWorkflowIdentityArgs(args, wfCtx.Runtime)
			return args, nil
		},
		CheckResult: func(_ *Context, result map[string]any) CheckOutcome {
			if existing, ok := firstCFS(result); ok {
				name := cfsString(existing, "Name")
				id := cfsString(existing, "CfsId", "CFSId")
				switch {
				case name != "" && id != "":
					return CheckFailed(fmt.Sprintf("该可用区已经有 CFS「%s」（%s）。平台每个账号在同一可用区只允许一个 CFS，请使用或扩容现有 CFS。", name, id))
				case id != "":
					return CheckFailed(fmt.Sprintf("该可用区已经有 CFS（%s）。平台每个账号在同一可用区只允许一个 CFS，请使用或扩容现有 CFS。", id))
				default:
					return CheckFailed("该可用区已经有 CFS。平台每个账号在同一可用区只允许一个 CFS，请使用或扩容现有 CFS。")
				}
			}
			return CheckPassed()
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
				"ChargeType": cfsChargeTypeLabel(cfsChargeType(wfCtx.Params)),
				"Quantity":   wfCtx.Params["Quantity"],
				"warning":    "将创建新的 CFS 共享文件存储并开始计费；删除能力不由 agent 暴露。",
			}
			priceText := cfsCreatePriceText(priceResult)
			if priceText == "" {
				return nil, fmt.Errorf("%s", missingWorkflowPriceMessage)
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
				"Name": wfCtx.Params["Name"],
				"Size": wfCtx.Params["Size"],
				// Region/Zone are still required by the deployed public gateway to
				// route CreateCFS. zone_id/az_group remain the authoritative placement
				// selected from the trusted zone snapshot; sending both is accepted by
				// the upstream BaseRequest and prevents gateway/version skew.
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
		CheckResult: func(wfCtx *Context, result map[string]any) CheckOutcome {
			cfs, ok := firstCFS(result)
			if !ok {
				return CheckFailed("未找到该 CFS。")
			}
			currentSize := cfsNumber(cfs, "Size")
			if currentSize <= 0 {
				return CheckFailed("未能识别当前 CFS 容量，无法安全扩容。")
			}
			targetSize := paramNum(wfCtx.Params, "Size", 0)
			if targetSize <= currentSize {
				return CheckFailed(fmt.Sprintf("目标容量必须大于当前容量：当前 %.0fGB，目标 %.0fGB。CFS 只支持扩容，不支持缩容或保持不变。", currentSize, targetSize))
			}
			zoneID := cfsNumber(cfs, "ZoneId", "ZoneID")
			if zoneID <= 0 {
				return CheckFailed("未获取到 CFS 所在可用区编号，无法安全执行扩容。")
			}
			wfCtx.Params["CurrentCFSSize"] = currentSize
			wfCtx.Params["CFSName"] = cfsString(cfs, "Name")
			wfCtx.Params["CFSZoneId"] = zoneID
			return CheckPassed()
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
	isPod, zoneID, azGroup, resolvedRegion, resolved, err := resolveCreateCFSZone(wfCtx, zone)
	if err != nil {
		return err
	}
	if resolved != "" {
		zone = resolved
	}
	// resolveCreateCFSZone fails closed on a record missing Region, so resolvedRegion
	// is always non-empty here: the catalog record is the sole Region source, never a
	// zone-string guess.
	region := resolvedRegion
	chargeType := cfsChargeType(wfCtx.Params)
	switch chargeType {
	case "Month", "Year", "Day", "Dynamic":
		// Dynamic is the upstream CFS wire value for its hourly/on-demand
		// product. It remains an internal protocol value; all user-facing cards
		// and resource renders call it 按量.
	case "Postpay":
		return fmt.Errorf("CFS 的按量计费需要使用当前产品支持的按量选项，请重新选择计费方式。")
	default:
		return fmt.Errorf("CFS 计费方式无效，请选择包月、包年、包日或按量。")
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
	// The turn's single zone catalog snapshot is authoritative: CFS resolves its
	// Pod-zone placement from it (no second support-zone query), keeping its own
	// Pod-only and internal-id checks on the record the snapshot returns.
	placement, err := workflowZonePlacement(wfCtx, requested)
	if err != nil {
		return false, 0, 0, "", "", err
	}
	if !placement.IsPod {
		return false, 0, 0, "", "", fmt.Errorf("CFS 当前只支持 Pod/容器可用区，%s 不是 Pod 区，不能创建 CFS。", requested)
	}
	if placement.ZoneID == 0 {
		return false, 0, 0, "", "", fmt.Errorf("未获取到可用区 %s 的内部编号，无法安全创建 CFS。", placement.Zone)
	}
	if placement.AzGroup == 0 {
		return false, 0, 0, "", "", fmt.Errorf("未获取到可用区 %s 的内部区域编号，无法安全创建 CFS。", placement.Zone)
	}
	if strings.TrimSpace(placement.Region) == "" {
		// Fail closed: a record the catalog carries but with no Region must NOT be
		// back-filled by trimming the zone string — that reintroduces the
		// split-source Region the snapshot exists to end.
		return false, 0, 0, "", "", fmt.Errorf("未获取到可用区 %s 的地域，无法安全创建 CFS。", placement.Zone)
	}
	return true, placement.ZoneID, placement.AzGroup, placement.Region, placement.Zone, nil
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

// cfsChargeTypeLabel is the single presentation boundary for the CFS-specific
// billing protocol. Dynamic must remain on the wire until the upstream API
// changes, but exposing that legacy enum to users would make one product appear
// to have two different on-demand modes.
func cfsChargeTypeLabel(chargeType string) string {
	switch strings.ToLower(strings.TrimSpace(chargeType)) {
	case "dynamic":
		return "按量"
	case "day":
		return "包日"
	case "month":
		return "包月"
	case "year":
		return "包年"
	default:
		return strings.TrimSpace(chargeType)
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
// PriceDetails array, which the frontend renders as "[object Object]". It emits
// bare "¥X.XX" because the confirm card already shows ChargeType/Quantity.
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
