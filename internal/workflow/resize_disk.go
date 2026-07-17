package workflow

import (
	"fmt"
	"strings"
)

const resizeDiskMissingTargetMessage = "扩已有盘需要指定目标容量（GB）。请告诉我要扩到多大，例如 120GB。"

func ResizeDiskDef() *Definition {
	return &Definition{
		Name:        "ResizeDiskWorkflow",
		Description: "查询实例 -> 检查扩盘条件 -> 查询扩盘价格 -> 确认扩盘 -> 扩已有盘",
		Steps: []Step{
			stepQueryForResizeDisk(),
			stepQuerySupportZonesForResizeDisk(),
			stepCheckResizeDisk(),
			stepQueryResizeDiskPrice(),
			stepConfirmResizeDisk(),
			stepResizeDisk(),
		},
		ResultData: func(wfCtx *Context) map[string]any {
			return map[string]any{
				"UHostId":        wfCtx.Params["UHostId"],
				"UDiskId":        wfCtx.Params["ResolvedDiskId"],
				"target_size_gb": wfCtx.Params["Size"],
			}
		},
	}
}

func stepQueryForResizeDisk() Step {
	return Step{
		Name: "查询实例",
		Type: StepToolCall,
		Tool: "DescribeCompShareInstance",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			size := paramNum(wfCtx.Params, "Size", 0)
			if size <= 0 {
				return nil, NewMissingSlotError(resizeDiskMissingTargetMessage, "target_size_gb")
			}
			wfCtx.Params["Size"] = size
			return map[string]any{
				"UHostIds": []any{wfCtx.Params["UHostId"]},
			}, nil
		},
		CheckResult: func(wfCtx *Context, result map[string]any) CheckOutcome {
			if extractInstanceState(result) == "" {
				return CheckFailed("未找到该实例。")
			}
			disk, err := resolveDiskForResize(wfCtx, result)
			if err != nil {
				return CheckFailed(err.Error())
			}
			targetSize := paramNum(wfCtx.Params, "Size", 0)
			currentSize := diskNumber(disk, "Size", "DiskSpace")
			if currentSize <= 0 {
				return CheckFailed("未能识别当前磁盘容量，无法安全扩容。")
			}
			if targetSize <= currentSize {
				return CheckFailed(fmt.Sprintf("目标容量必须大于当前容量：当前 %.0fGB，目标 %.0fGB。扩已有盘只支持扩容，不能缩容或保持不变。", currentSize, targetSize))
			}
			diskID := diskIDValue(disk)
			wfCtx.Params["ResolvedDiskId"] = diskID
			wfCtx.Params["ResolvedDiskName"] = diskString(disk, "Name")
			wfCtx.Params["ResolvedDiskRole"] = diskRole(disk)
			wfCtx.Params["ResolvedDiskType"] = diskString(disk, "DiskType")
			wfCtx.Params["CurrentDiskSize"] = currentSize
			return CheckPassed()
		},
	}
}

func stepQuerySupportZonesForResizeDisk() Step {
	return Step{
		Name:     "查询支持区",
		Type:     StepToolCall,
		Tool:     "DescribeCompShareSupportZone",
		Optional: true,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
}

func stepCheckResizeDisk() Step {
	return Step{
		Name: "检查扩盘条件",
		Type: StepToolCall,
		Tool: "CheckCompShareResizeAttachedDisk",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			queried := wfCtx.Result("查询实例")
			args := map[string]any{
				"UHostId":   wfCtx.Params["UHostId"],
				"DiskId":    wfCtx.Params["ResolvedDiskId"],
				"DiskSpace": wfCtx.Params["Size"],
			}
			if _, err := addRequiredPodPlacementArgs(args, queried, wfCtx.Result("查询支持区")); err != nil {
				return nil, err
			}
			return args, nil
		},
		CheckResult: func(wfCtx *Context, result map[string]any) CheckOutcome {
			if needRestart, ok := result["NeedRestart"].(bool); ok {
				wfCtx.Params["NeedRestart"] = needRestart
			}
			if diskID, ok := result["DiskId"].(string); ok && diskID != "" {
				wfCtx.Params["ResolvedDiskId"] = diskID
			}
			return CheckPassed()
		},
	}
}

func stepQueryResizeDiskPrice() Step {
	return Step{
		Name: "查询扩盘价格",
		Type: StepToolCall,
		Tool: "GetCompShareAttachedDiskUpgradePrice",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			queried := wfCtx.Result("查询实例")
			args := map[string]any{
				"UHostId":   wfCtx.Params["UHostId"],
				"DiskId":    wfCtx.Params["ResolvedDiskId"],
				"DiskSpace": wfCtx.Params["Size"],
			}
			if _, err := addRequiredPodPlacementArgs(args, queried, wfCtx.Result("查询支持区")); err != nil {
				return nil, err
			}
			return args, nil
		},
	}
}

func stepConfirmResizeDisk() Step {
	return Step{
		Name: "确认扩盘",
		Type: StepConfirm,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			summary := extractInstanceSummary(wfCtx.Result("查询实例"))
			summary["disk_id"] = wfCtx.Params["ResolvedDiskId"]
			summary["disk_name"] = wfCtx.Params["ResolvedDiskName"]
			summary["disk_role"] = wfCtx.Params["ResolvedDiskRole"]
			summary["disk_type"] = wfCtx.Params["ResolvedDiskType"]
			summary["current_size_gb"] = wfCtx.Params["CurrentDiskSize"]
			summary["target_size_gb"] = wfCtx.Params["Size"]
			if needRestart, ok := wfCtx.Params["NeedRestart"].(bool); ok {
				summary["need_restart"] = needRestart
			}
			priceResult := wfCtx.Result("查询扩盘价格")
			price, err := requiredPriceField(priceResult, "Price")
			if err != nil {
				return nil, err
			}
			summary["price_delta"] = price
			if originalPrice, ok := priceResult["OriginalPrice"]; ok {
				summary["original_price_delta"] = originalPrice
			}
			if listPrice, ok := priceResult["ListPrice"]; ok {
				summary["list_price_delta"] = listPrice
			}
			summary["warning"] = "将把这块已有磁盘扩容到目标容量；Size 是目标容量，不是新增容量。扩容后不能缩小，系统内可能还需要扩展分区或文件系统。"
			return summary, nil
		},
	}
}

func stepResizeDisk() Step {
	return Step{
		Name: "扩已有盘",
		Type: StepToolCall,
		ToolFunc: func(wfCtx *Context) string {
			if resizeDiskViaInstance(wfCtx.Result("查询实例")) {
				return "ResizeCompShareInstance"
			}
			return "ResizeCompShareDisk"
		},
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			queried := wfCtx.Result("查询实例")
			if resizeDiskViaInstance(queried) {
				args := map[string]any{
					"UHostId":   wfCtx.Params["UHostId"],
					"DiskId":    wfCtx.Params["ResolvedDiskId"],
					"DiskSpace": wfCtx.Params["Size"],
				}
				if _, err := addRequiredPodPlacementArgs(args, queried, wfCtx.Result("查询支持区")); err != nil {
					return nil, err
				}
				return args, nil
			}
			args := map[string]any{
				"UHostId": wfCtx.Params["UHostId"],
				"UDiskId": wfCtx.Params["ResolvedDiskId"],
				"Size":    wfCtx.Params["Size"],
			}
			if _, err := addRequiredPodPlacementArgs(args, queried, wfCtx.Result("查询支持区")); err != nil {
				return nil, err
			}
			return args, nil
		},
	}
}

func resizeDiskViaInstance(result map[string]any) bool {
	return isPodInstanceResult(result) || isContainerInstanceResult(result)
}

func resolveDiskForResize(wfCtx *Context, result map[string]any) (map[string]any, error) {
	disks := extractDiskSet(result)
	if len(disks) == 0 {
		return nil, fmt.Errorf("该实例没有可识别的磁盘列表，无法扩已有盘。")
	}

	if diskID := paramStr(wfCtx.Params, "DiskId", ""); diskID != "" {
		return findDiskByID(disks, diskID)
	}
	if diskID := paramStr(wfCtx.Params, "UDiskId", ""); diskID != "" {
		return findDiskByID(disks, diskID)
	}

	switch normalizeDiskRoleParam(paramStr(wfCtx.Params, "DiskType", "")) {
	case "boot":
		for _, disk := range disks {
			if isBootDisk(disk) {
				return disk, nil
			}
		}
		return nil, fmt.Errorf("未在该实例上找到系统盘。")
	case "data":
		dataDisks := make([]map[string]any, 0)
		for _, disk := range disks {
			if isDataDisk(disk) {
				dataDisks = append(dataDisks, disk)
			}
		}
		switch len(dataDisks) {
		case 0:
			return nil, fmt.Errorf("未在该实例上找到数据盘。")
		case 1:
			return dataDisks[0], nil
		default:
			return nil, fmt.Errorf("该实例有多块数据盘，请指定要扩容的 DiskId。")
		}
	default:
		return nil, fmt.Errorf("请指定要扩系统盘还是数据盘；如果是数据盘，请提供 DiskId。")
	}
}

func extractDiskSet(result map[string]any) []map[string]any {
	host, ok := firstInstance(result)
	if !ok {
		return nil
	}
	rawDisks, ok := host["DiskSet"].([]any)
	if !ok {
		return nil
	}
	disks := make([]map[string]any, 0, len(rawDisks))
	for _, raw := range rawDisks {
		if disk, ok := raw.(map[string]any); ok {
			disks = append(disks, disk)
		}
	}
	return disks
}

func firstInstance(result map[string]any) (map[string]any, bool) {
	if result == nil {
		return nil, false
	}
	hostSet, ok := result["UHostSet"].([]any)
	if !ok || len(hostSet) == 0 {
		return nil, false
	}
	host, ok := hostSet[0].(map[string]any)
	return host, ok
}

func findDiskByID(disks []map[string]any, want string) (map[string]any, error) {
	for _, disk := range disks {
		for _, key := range []string{"DiskId", "UDiskId", "Id", "DiskShortId"} {
			if diskString(disk, key) == want {
				return disk, nil
			}
		}
	}
	return nil, fmt.Errorf("未在该实例上找到 DiskId=%s 的磁盘。", want)
}

func diskIDValue(disk map[string]any) string {
	for _, key := range []string{"DiskId", "UDiskId", "Id"} {
		if v := diskString(disk, key); v != "" {
			return v
		}
	}
	return ""
}

func diskString(disk map[string]any, key string) string {
	if disk == nil {
		return ""
	}
	if v, ok := disk[key].(string); ok {
		return v
	}
	return ""
}

func diskNumber(disk map[string]any, keys ...string) float64 {
	if disk == nil {
		return 0
	}
	for _, key := range keys {
		switch v := disk[key].(type) {
		case float64:
			return v
		case float32:
			return float64(v)
		case int:
			return float64(v)
		case int64:
			return float64(v)
		case uint32:
			return float64(v)
		case uint64:
			return float64(v)
		}
	}
	return 0
}

func normalizeDiskRoleParam(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "boot", "system", "systemdisk", "sys", "系统盘":
		return "boot"
	case "data", "udisk", "datasdisk", "datadisk", "数据盘":
		return "data"
	default:
		return ""
	}
}

func isBootDisk(disk map[string]any) bool {
	if strings.EqualFold(diskString(disk, "Type"), "Boot") {
		return true
	}
	return strings.EqualFold(diskString(disk, "IsBoot"), "True")
}

func isDataDisk(disk map[string]any) bool {
	if isBootDisk(disk) {
		return false
	}
	t := diskString(disk, "Type")
	return strings.EqualFold(t, "Data") || strings.EqualFold(t, "Udisk") || t == ""
}

func diskRole(disk map[string]any) string {
	if isBootDisk(disk) {
		return "Boot"
	}
	if strings.EqualFold(diskString(disk, "Type"), "Udisk") {
		return "Udisk"
	}
	return "Data"
}
