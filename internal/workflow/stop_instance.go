package workflow

// StopInstanceDef returns the 3-step workflow definition for stopping a
// CompShare GPU instance: query state, confirm shutdown, then stop.
func StopInstanceDef() *Definition {
	return &Definition{
		Name:        "StopInstanceWorkflow",
		Description: "查询实例 → 确认关机 → 关机",
		Steps: []Step{
			stepQueryInstance(),
			stepConfirmStop(),
			stepStopInstance(),
		},
	}
}

// ---------------------------------------------------------------------------
// Step definitions
// ---------------------------------------------------------------------------

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
		CheckResult: func(_ *Context, result map[string]any) (bool, string) {
			state := extractInstanceState(result)
			switch state {
			case "":
				return false, "未找到该实例。"
			case "Stopped":
				return false, "实例已经是关机状态，无需操作。"
			case "Running":
				return true, ""
			default:
				return false, "实例当前状态为「" + state + "」，仅 Running 状态可以关机。"
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
			summary["warning"] = "关机后实例和 GPU 停止计费。系统盘 100GB 免费；如挂载数据盘或系统盘扩容超出 100GB，仍会产生磁盘费用。如需彻底停止计费请到控制台释放实例。"
			return summary, nil
		},
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
				"Region":  extractInstanceRegion(queried, defaultRegion),
				"Zone":    extractInstanceZone(queried, defaultZone),
				"UHostId": wfCtx.Params["UHostId"],
			}
			if extractChargeType(queried) == "Spot" {
				args["Force"] = true
			}
			return args, nil
		},
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

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

func extractChargeType(result map[string]any) string {
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
	if ct, ok := first["ChargeType"].(string); ok {
		return ct
	}
	return ""
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
