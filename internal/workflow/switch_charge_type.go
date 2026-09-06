package workflow

import (
	"fmt"
	"strings"
)

const (
	switchChargeTypeQueryStep    = "查询实例"
	switchChargeTypePriceStep    = "查询目标计费价格"
	switchChargeTypeWriteStep    = "切换计费方式"
	switchChargeTypeReadbackStep = "回读计费方式"
)

// SwitchChargeTypeDef changes a running, non-spot GPU instance from Postpay to
// one of the prepaid modes accepted by the upstream SwitchChargeType API.
func SwitchChargeTypeDef() *Definition {
	return &Definition{
		Name: "SwitchChargeTypeWorkflow",
		Steps: []Step{
			stepQueryForSwitchChargeType(),
			stepQuerySupportZones(),
			stepQuerySwitchChargeTypePrice(),
			stepConfirmSwitchChargeType(),
			stepSwitchChargeType(),
			stepReadbackSwitchChargeType(),
		},
		ResultData: switchChargeTypeResultData,
	}
}

func stepQuerySwitchChargeTypePrice() Step {
	return Step{
		Name: switchChargeTypePriceStep,
		Type: StepToolCall,
		Tool: "GetCompShareInstanceUserPrice",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			target, err := switchChargeTypeTarget(wfCtx.Params)
			if err != nil {
				return nil, err
			}
			host := firstUHost(wfCtx.Result(switchChargeTypeQueryStep))
			if host == nil {
				return nil, fmt.Errorf("未获取到实例实时规格，无法查询目标计费价格。")
			}
			disks, err := resizeCapacityDisks(host)
			if err != nil {
				return nil, fmt.Errorf("实例实时磁盘配置不完整，无法查询目标计费价格。")
			}
			gpuType := strings.TrimSpace(stringFieldAny(host["GpuType"]))
			gpu := firstNumberField(host, "GPU", "Gpu")
			cpu := firstNumberField(host, "CPU", "Cpu")
			memory := firstNumberField(host, "Memory")
			if gpuType == "" || gpu <= 0 || cpu <= 0 || memory <= 0 {
				return nil, fmt.Errorf("实例实时规格不完整，无法查询目标计费价格。")
			}
			// Mirror the production console's observed instance and complete
			// DiskSet. The requested target is included because the upstream
			// endpoint's default all-modes response omits Year; a usable target
			// row in the response still decides whether confirmation is possible.
			return addRequiredPodPlacementArgs(map[string]any{
				"GpuType": gpuType, "GPU": gpu, "CPU": cpu, "Memory": memory,
				"ChargeType": target, "Disks": disks,
			}, wfCtx.Result(switchChargeTypeQueryStep), wfCtx.Result("查询支持区"))
		},
		CheckResult: func(wfCtx *Context, result map[string]any) CheckOutcome {
			target, err := switchChargeTypeTarget(wfCtx.Params)
			if err != nil {
				return CheckFailed(err.Error())
			}
			if _, _, ok := switchChargeTypePriceParts(result, target); !ok {
				return CheckFailed("未获取到目标计费方式的实例和系统盘价格，无法发起付费确认。请稍后重试或到控制台确认费用。")
			}
			return CheckPassed()
		},
	}
}

func stepQueryForSwitchChargeType() Step {
	return Step{
		Name: switchChargeTypeQueryStep,
		Type: StepToolCall,
		Tool: "DescribeCompShareInstance",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			if _, err := switchChargeTypeTarget(wfCtx.Params); err != nil {
				return nil, err
			}
			return map[string]any{"UHostIds": []any{wfCtx.Params["UHostId"]}}, nil
		},
		CheckResult: func(wfCtx *Context, result map[string]any) CheckOutcome {
			id := strings.TrimSpace(paramStr(wfCtx.Params, "UHostId", ""))
			if id == "" || !narrowInstanceResultToUHostID(result, id) {
				return CheckFailed("未找到该实例。")
			}
			host := firstUHost(result)
			if !strings.EqualFold(strings.TrimSpace(stringFieldAny(host["State"])), "Running") {
				return CheckFailed("实例必须处于运行状态才能切换计费方式。")
			}
			if value, ok := host["IsSpot"].(bool); ok && value {
				return CheckFailed("抢占式实例不支持切换计费方式。")
			}
			if firstNumberField(host, "GPU", "Gpu") <= 0 {
				return CheckFailed("无卡实例不支持切换计费方式。")
			}
			current := strings.TrimSpace(stringFieldAny(host["ChargeType"]))
			if current == "" {
				return CheckFailed("未能读取实例当前计费方式，无法发起切换。")
			}
			if !strings.EqualFold(current, "Postpay") {
				return CheckFailed(fmt.Sprintf("实例当前计费方式为「%s」，此操作仅支持从按量后付费切换为预付费。", ChargeTypeLabel(current)))
			}
			wfCtx.Params["InitialChargeType"] = current
			return CheckPassed()
		},
	}
}

func stepConfirmSwitchChargeType() Step {
	return Step{
		Name: "确认计费方式",
		Type: StepConfirm,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			target, err := switchChargeTypeTarget(wfCtx.Params)
			if err != nil {
				return nil, err
			}
			instancePrice, systemDiskPrice, ok := switchChargeTypePriceParts(
				wfCtx.Result(switchChargeTypePriceStep), target,
			)
			if !ok {
				return nil, fmt.Errorf("未获取到目标计费方式的实例和系统盘价格，无法发起付费确认。请稍后重试或到控制台确认费用。")
			}
			summary := extractInstanceSummary(wfCtx.Result(switchChargeTypeQueryStep))
			current := strings.TrimSpace(paramStr(wfCtx.Params, "InitialChargeType", ""))
			summary["计费方式变更"] = ChargeTypeLabel(current) + " → " + ChargeTypeLabel(target)
			summary["目标实例价格"] = switchChargeTypePriceText(instancePrice, target)
			summary["目标系统盘价格"] = switchChargeTypePriceText(systemDiskPrice, target)
			summary["目标合计价格"] = switchChargeTypePriceText(instancePrice+systemDiskPrice, target)
			delete(summary, "ChargeType")
			summary["warning"] = "确认后将切换实例及系统盘的计费方式；价格为当前查询结果，最终费用和到期时间以平台账单为准。当前接口及 Agent 不支持直接切回按量后付费，后续是否可切回以控制台和平台实际支持为准。"
			if isPodInstanceResult(wfCtx.Result(switchChargeTypeQueryStep)) {
				summary["warning"] = "确认后将切换实例及其关联云存储的计费方式；以上系统盘报价来自当前实例详情，最终费用和到期时间以平台账单为准。当前接口及 Agent 不支持直接切回按量后付费，后续是否可切回以控制台和平台实际支持为准。"
			}
			return summary, nil
		},
	}
}

// switchChargeTypePriceParts follows the production console contract: only the
// instance and system-disk components participate in this quote. For Pod the
// system-disk component is the CVolume price, not an additional UDisk charge.
// PriceDetails may report those components in separate rows, so they are resolved
// independently for the requested target mode. Data-disk prices are excluded.
// Both components are required because the upstream switch mutates compute and
// associated storage together; a partial response is not a usable quote.
func switchChargeTypePriceParts(result map[string]any, target string) (instance, systemDisk float64, ok bool) {
	details, _ := result["PriceDetails"].([]any)
	instanceFound, systemDiskFound := false, false
	for _, raw := range details {
		row, _ := raw.(map[string]any)
		if row == nil || !strings.EqualFold(strings.TrimSpace(stringFieldAny(row["ChargeType"])), target) {
			continue
		}
		if !instanceFound {
			instance, instanceFound = priceNumber(row["Instance"])
		}
		if !systemDiskFound {
			systemDisk, systemDiskFound = priceNumber(row["SystemDisks"])
		}
	}
	return instance, systemDisk, instanceFound && systemDiskFound
}

func switchChargeTypePriceText(amount float64, chargeType string) string {
	return fmt.Sprintf("¥%.2f%s%s", amount, chargePeriodUnit(chargeType), estimatedPriceSuffix)
}

func stepSwitchChargeType() Step {
	return Step{
		Name: switchChargeTypeWriteStep,
		Type: StepToolCall,
		Tool: "SwitchChargeType",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			target, err := switchChargeTypeTarget(wfCtx.Params)
			if err != nil {
				return nil, err
			}
			return addRequiredPodPlacementArgs(map[string]any{
				"UHostId":        wfCtx.Params["UHostId"],
				"DestChargeType": target,
			}, wfCtx.Result(switchChargeTypeQueryStep), wfCtx.Result("查询支持区"))
		},
	}
}

func stepReadbackSwitchChargeType() Step {
	return Step{
		Name:     switchChargeTypeReadbackStep,
		Type:     StepToolCall,
		Tool:     "DescribeCompShareInstance",
		Optional: true,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			return map[string]any{"UHostIds": []any{wfCtx.Params["UHostId"]}}, nil
		},
		CheckResult: func(wfCtx *Context, result map[string]any) CheckOutcome {
			if narrowInstanceResultToUHostID(result, paramStr(wfCtx.Params, "UHostId", "")) {
				return CheckPassed()
			}
			return CheckFailed("切换请求已提交，但未能回读目标实例。")
		},
	}
}

func switchChargeTypeResultData(wfCtx *Context) map[string]any {
	target, _ := switchChargeTypeTarget(wfCtx.Params)
	data := map[string]any{
		"UHostId":            wfCtx.Params["UHostId"],
		"PreviousChargeType": paramStr(wfCtx.Params, "InitialChargeType", ""),
		"TargetChargeType":   target,
	}
	readback := wfCtx.Result(switchChargeTypeReadbackStep)
	host := firstUHost(readback)
	if host == nil {
		data["ReadbackAvailable"] = false
		data["Verified"] = false
		return data
	}
	observed := strings.TrimSpace(stringFieldAny(host["ChargeType"]))
	data["ReadbackAvailable"] = true
	data["ObservedChargeType"] = observed
	data["Verified"] = observed != "" && strings.EqualFold(observed, target)
	return data
}

func switchChargeTypeTarget(params map[string]any) (string, error) {
	target := strings.TrimSpace(paramStr(params, "DestChargeType", ""))
	switch target {
	case "Dynamic", "Day", "Month", "Year":
		return target, nil
	default:
		return "", NewMissingSlotError("请选择目标计费方式：包时、包日、包月或包年。", "dest_charge_type")
	}
}

// ChargeTypeLabel is the shared presentation boundary for upstream billing
// mode values used by workflow cards and deterministic delivery replies.
func ChargeTypeLabel(value string) string {
	if label := chargeTypeLabel(value); label != strings.TrimSpace(value) {
		return label
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "dynamic":
		return "包时"
	case "year":
		return "包年"
	default:
		return strings.TrimSpace(value)
	}
}
