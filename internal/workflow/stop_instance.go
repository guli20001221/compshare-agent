package workflow

import "strings"

// StopInstanceDef submits one stop request after querying and confirmation.
// Upstream stopping is asynchronous, so request acceptance is not presented as
// a verified Stopped state.
func StopInstanceDef() *Definition {
	return &Definition{
		Name: "StopInstanceWorkflow",
		Steps: []Step{
			stepQueryInstance(),
			stepQuerySupportZones(),
			stepConfirmStop(),
			stepStopInstance(),
		},
	}
}
func stepQueryInstance() Step {
	return Step{
		Name: "查询实例",
		Type: StepToolCall,
		Tool: "DescribeCompShareInstance",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			return map[string]any{
				"UHostIds": []any{wfCtx.Params["UHostId"]},
			}, nil
		},
		CheckResult: func(_ *Context, result map[string]any) CheckOutcome {
			state := extractInstanceState(result)
			switch state {
			case "":
				return CheckFailed("未找到该实例。")
			case "Stopped":
				return CheckFailed("实例已经是关机状态，无需操作。")
			case "Running":
				return CheckPassed()
			default:
				return CheckFailed("实例当前状态为「" + state + "」，仅 Running 状态可以关机。")
			}
		},
	}
}

func stepConfirmStop() Step {
	return Step{
		Name: "确认关机",
		Type: StepConfirm,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			summary := extractInstanceSummary(wfCtx.Result("查询实例"))
			summary["warning"] = stopBillingWarning(summary)
			return summary, nil
		},
	}
}

func stopBillingWarning(summary map[string]any) string {
	chargeType := strings.TrimSpace(stringFieldAny(summary["ChargeType"]))
	diskNote := "磁盘及其他保留资源可能继续计费，具体以控制台价格详情为准。"
	switch strings.ToLower(chargeType) {
	case "postpay", "spot", "preemptive":
		return "关机会结束当前实例/GPU 的运行计费段；" + diskNote + "如需释放全部保留资源，请到控制台释放实例。"
	case "month", "year", "day", "dynamic":
		return "关机不会取消或退款已购买的计费周期，实例/GPU 契约仍会保留；" + diskNote + "如需停止后续费用，请按控制台规则退订或释放实例。"
	default:
		return "关机会中断实例内任务，但不能据此确认费用已经停止；" + diskNote + "如需停止后续费用，请到控制台核对计费方式并释放实例。"
	}
}

func stepStopInstance() Step {
	return Step{
		Name: "关机",
		Type: StepToolCall,
		Tool: "StopCompShareInstance",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			queried := wfCtx.Result("查询实例")
			args := map[string]any{
				"UHostId": wfCtx.Params["UHostId"],
			}
			return addRequiredPodPlacementArgs(args, queried, wfCtx.Result("查询支持区"))
		},
	}
}
// extractInstanceZone returns the Zone from the first UHostSet entry, or defaultVal.
func extractInstanceZone(result map[string]any, defaultVal string) string {
	if result == nil {
		return defaultVal
	}
	hostSet, ok := result["UHostSet"].([]any)
	if !ok || len(hostSet) == 0 {
		return defaultVal
	}
	first, ok := hostSet[0].(map[string]any)
	if !ok {
		return defaultVal
	}
	if zone, ok := first["Zone"].(string); ok && zone != "" {
		return zone
	}
	return defaultVal
}

// extractInstanceState returns the State field from the first entry in UHostSet,
// or an empty string if the result is missing or malformed.
func extractInstanceState(result map[string]any) string {
	if result == nil {
		return ""
	}
	hostSet, ok := result["UHostSet"].([]any)
	if !ok || len(hostSet) == 0 {
		return ""
	}
	first, ok := hostSet[0].(map[string]any)
	if !ok {
		return ""
	}
	if state, ok := first["State"].(string); ok {
		return state
	}
	return ""
}

func extractInstanceName(result map[string]any) string {
	if result == nil {
		return ""
	}
	hostSet, ok := result["UHostSet"].([]any)
	if !ok || len(hostSet) == 0 {
		return ""
	}
	first, ok := hostSet[0].(map[string]any)
	if !ok {
		return ""
	}
	if name, ok := first["Name"].(string); ok {
		return name
	}
	return ""
}

// extractInstanceSummary builds a summary map from the first UHostSet entry,
// including UHostId, Name, State, GpuType, GPU, and ChargeType.
func extractInstanceSummary(result map[string]any) map[string]any {
	summary := make(map[string]any)
	if result == nil {
		return summary
	}
	hostSet, ok := result["UHostSet"].([]any)
	if !ok || len(hostSet) == 0 {
		return summary
	}
	first, ok := hostSet[0].(map[string]any)
	if !ok {
		return summary
	}
	for _, key := range []string{"UHostId", "Name", "State", "GpuType", "GPU", "ChargeType"} {
		if v, exists := first[key]; exists {
			summary[key] = v
		}
	}
	return summary
}
