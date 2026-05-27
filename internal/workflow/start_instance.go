package workflow

import "fmt"

// StartInstanceDef returns the 3-step workflow definition for starting a
// CompShare GPU instance: query instance, confirm start, then start.
func StartInstanceDef() *Definition {
	return &Definition{
		Name:        "StartInstanceWorkflow",
		Description: "查询实例 → 确认开机 → 开机",
		Steps: []Step{
			stepQueryForStart(),
			stepConfirmStart(),
			stepStartInstance(),
		},
	}
}

// ---------------------------------------------------------------------------
// Step definitions
// ---------------------------------------------------------------------------

func stepQueryForStart() Step {
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
			case "Stopped":
				return true, ""
			case "":
				return false, "未找到该实例。"
			case "Running":
				return false, "实例当前已处于运行状态，无需重复开机。"
			case "Starting":
				return false, "实例正在启动中，请稍等。"
			default:
				return false, fmt.Sprintf("实例当前状态为「%s」，仅 Stopped 状态可以开机。", state)
			}
		},
	}
}

func stepConfirmStart() Step {
	return Step{
		Name: "确认开机",
		Type: StepConfirm,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			summary := extractInstanceSummary(wfCtx.Result("查询实例"))
			if wg, ok := wfCtx.Params["WithoutGpu"]; ok && wg == true {
				summary["mode"] = "无卡模式（不分配 GPU，仅用于数据访问/维护）"
			}
			return summary, nil
		},
	}
}

func stepStartInstance() Step {
	return Step{
		Name: "开机",
		Type: StepToolCall,
		Tool: "StartCompShareInstance",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			queried := wfCtx.Result("查询实例")
			args := map[string]any{
				"Region":  extractInstanceRegion(queried, defaultRegion),
				"Zone":    extractInstanceZone(queried, defaultZone),
				"UHostId": wfCtx.Params["UHostId"],
			}
			if wg, ok := wfCtx.Params["WithoutGpu"]; ok && wg == true {
				args["WithoutGpu"] = true
			}
			return args, nil
		},
	}
}
