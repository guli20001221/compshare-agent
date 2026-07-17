package workflow

// RebootInstanceDef returns the 3-step workflow definition for rebooting a
// CompShare GPU instance: query state, confirm reboot, then reboot.
func RebootInstanceDef() *Definition {
	return &Definition{
		Name:        "RebootInstanceWorkflow",
		Description: "查询实例 → 确认重启 → 重启",
		Steps: []Step{
			stepQueryForReboot(),
			stepQuerySupportZones(),
			stepConfirmReboot(),
			stepRebootInstance(),
		},
	}
}

// ---------------------------------------------------------------------------
// Step definitions
// ---------------------------------------------------------------------------

func stepQueryForReboot() Step {
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
			case "Running":
				return CheckPassed()
			case "Stopped":
				return CheckFailed("实例当前是关机状态，无法重启。请先开机。")
			case "Rebooting":
				return CheckFailed("实例正在重启中，请稍等。")
			default:
				return CheckFailed("实例当前状态为「" + state + "」，仅 Running 状态可以重启。")
			}
		},
	}
}

func stepConfirmReboot() Step {
	return Step{
		Name: "确认重启",
		Type: StepConfirm,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			summary := extractInstanceSummary(wfCtx.Result("查询实例"))
			summary["warning"] = "重启会中断当前运行的任务，请确保已保存工作。"
			return summary, nil
		},
	}
}

func stepRebootInstance() Step {
	return Step{
		Name: "重启",
		Type: StepToolCall,
		Tool: "RebootCompShareInstance",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			queried := wfCtx.Result("查询实例")
			return addRequiredPodPlacementArgs(map[string]any{
				"UHostId": wfCtx.Params["UHostId"],
			}, queried, wfCtx.Result("查询支持区"))
		},
	}
}
