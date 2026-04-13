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
		CheckResult: func(result map[string]any) (bool, string) {
			state := extractInstanceState(result)
			if state == "Stopped" {
				return false, "实例已经是关机状态，无需操作。"
			}
			return true, ""
		},
	}
}

func stepConfirmStop() Step {
	return Step{
		Name: "确认关机",
		Type: StepConfirm,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			summary := extractInstanceSummary(wfCtx.Result("查询实例"))
			summary["warning"] = "关机后磁盘费用仍会产生，如需彻底停止计费请到控制台释放实例。"
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
			// Zone + UHostId required per docs/api/instance/StopCompShareInstance.md
			return map[string]any{
				"Zone":    extractInstanceZone(wfCtx.Result("查询实例"), defaultZone),
				"UHostId": wfCtx.Params["UHostId"],
			}, nil
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
