package workflow

import (
	"fmt"
	"strings"
)

const (
	switchChargeTypeQueryStep    = "查询实例"
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
			stepConfirmSwitchChargeType(),
			stepSwitchChargeType(),
			stepReadbackSwitchChargeType(),
		},
		ResultData: switchChargeTypeResultData,
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
			summary := extractInstanceSummary(wfCtx.Result(switchChargeTypeQueryStep))
			current := strings.TrimSpace(paramStr(wfCtx.Params, "InitialChargeType", ""))
			summary["计费方式变更"] = ChargeTypeLabel(current) + " → " + ChargeTypeLabel(target)
			delete(summary, "ChargeType")
			summary["warning"] = "确认后将切换实例计费方式；本接口不提供切换价格，实际费用和到期时间以平台账单为准。该操作不支持再切回按量后付费。"
			return summary, nil
		},
	}
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
