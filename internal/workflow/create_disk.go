package workflow

func CreateDiskDef() *Definition {
	return &Definition{
		Name:        "CreateDiskWorkflow",
		Description: "查询实例 → 确认创建数据盘 → 创建并挂载",
		Steps: []Step{
			stepQueryForDisk(),
			stepConfirmCreateDisk(),
			stepCreateAndAttachDisk(),
		},
	}
}

func stepQueryForDisk() Step {
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

func stepConfirmCreateDisk() Step {
	return Step{
		Name: "确认创建数据盘",
		Type: StepConfirm,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			summary := extractInstanceSummary(wfCtx.Result("查询实例"))
			summary["disk_size_gb"] = wfCtx.Params["Size"]
			summary["disk_type"] = "SSDDataDisk"
			summary["charge_type"] = "Dynamic"
			summary["warning"] = "将创建一块 SSD 云数据盘并挂载到该实例，按量计费。"
			return summary, nil
		},
	}
}

func stepCreateAndAttachDisk() Step {
	return Step{
		Name: "创建并挂载数据盘",
		Type: StepToolCall,
		Tool: "CreateAndAttachCompshareDisk",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			queried := wfCtx.Result("查询实例")
			name := extractInstanceName(queried)
			if name == "" {
				name = "data-disk"
			}
			args := map[string]any{
				"Region":     extractInstanceRegion(queried, defaultRegion),
				"Zone":       extractInstanceZone(queried, defaultZone),
				"UHostId":    wfCtx.Params["UHostId"],
				"Size":       wfCtx.Params["Size"],
				"Name":       name + "-data",
				"DiskType":   "SSDDataDisk",
				"ChargeType": "Dynamic",
			}
			return args, nil
		},
	}
}
