package workflow

// RenameInstanceDef returns the 3-step workflow definition for renaming a
// CompShare GPU instance: query instance, confirm rename, then execute.
func RenameInstanceDef() *Definition {
	return &Definition{
		Name:        "RenameInstanceWorkflow",
		Description: "查询实例 → 确认改名 → 修改名称",
		Steps: []Step{
			stepQueryForRename(),
			stepConfirmRename(),
			stepRenameInstance(),
		},
	}
}

// ---------------------------------------------------------------------------
// Step definitions
// ---------------------------------------------------------------------------

func stepQueryForRename() Step {
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
			if state == "" {
				return false, "未找到该实例。"
			}
			return true, ""
		},
	}
}

func stepConfirmRename() Step {
	return Step{
		Name: "确认改名",
		Type: StepConfirm,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			summary := extractInstanceSummary(wfCtx.Result("查询实例"))
			summary["NewName"] = wfCtx.Params["Name"]
			return summary, nil
		},
	}
}

func stepRenameInstance() Step {
	return Step{
		Name: "修改名称",
		Type: StepToolCall,
		Tool: "ModifyCompShareInstanceName",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			queried := wfCtx.Result("查询实例")
			return map[string]any{
				"Region":  extractInstanceRegion(queried, defaultRegion),
				"Zone":    extractInstanceZone(queried, defaultZone),
				"UHostId": wfCtx.Params["UHostId"],
				"Name":    wfCtx.Params["Name"],
			}, nil
		},
	}
}
