package workflow

import "fmt"

func ResizeInstanceDef() *Definition {
	return &Definition{
		Name:        "ResizeInstanceWorkflow",
		Description: "查询实例 → 查询变配价格 → 确认变配 → 变配",
		Steps: []Step{
			stepQueryForResize(),
			stepQueryResizePrice(),
			stepConfirmResize(),
			stepResizeInstance(),
		},
	}
}

func stepQueryForResize() Step {
	return Step{
		Name: "查询实例",
		Type: StepToolCall,
		Tool: "DescribeCompShareInstance",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			_, hasCpu := wfCtx.Params["Cpu"]
			_, hasGpu := wfCtx.Params["Gpu"]
			_, hasMem := wfCtx.Params["Memory"]
			if !hasCpu && !hasGpu && !hasMem {
				return nil, fmt.Errorf("变配请求必须至少指定 Cpu、Gpu、Memory 之一")
			}
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
				return false, "实例当前正在运行，变配需要先关机。"
			case "Stopping":
				return false, "实例正在关机中，请稍后再试。"
			default:
				return false, fmt.Sprintf("实例当前状态为「%s」，仅 Stopped 状态可以变配。", state)
			}
		},
	}
}

func stepQueryResizePrice() Step {
	return Step{
		Name: "查询变配价格",
		Type: StepToolCall,
		Tool: "GetCompShareInstanceUpgradePrice",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			args := map[string]any{
				"UHostId": wfCtx.Params["UHostId"],
			}
			if cpu, ok := wfCtx.Params["Cpu"]; ok {
				args["CPU"] = cpu
			}
			if gpu, ok := wfCtx.Params["Gpu"]; ok {
				args["GPU"] = gpu
			}
			if mem, ok := wfCtx.Params["Memory"]; ok {
				args["Memory"] = mem
			}
			return args, nil
		},
	}
}

func stepConfirmResize() Step {
	return Step{
		Name: "确认变配",
		Type: StepConfirm,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			summary := extractInstanceSummary(wfCtx.Result("查询实例"))
			if cpu, ok := wfCtx.Params["Cpu"]; ok {
				summary["target_cpu"] = cpu
			}
			if gpu, ok := wfCtx.Params["Gpu"]; ok {
				summary["target_gpu"] = gpu
			}
			if mem, ok := wfCtx.Params["Memory"]; ok {
				summary["target_memory"] = mem
			}
			priceResult := wfCtx.Result("查询变配价格")
			if price, ok := priceResult["Price"]; ok {
				summary["price_delta"] = price
			}
			summary["warning"] = "变配会修改实例的 CPU/GPU/内存配置，可能影响计费。"
			return summary, nil
		},
	}
}

func stepResizeInstance() Step {
	return Step{
		Name: "变配",
		Type: StepToolCall,
		Tool: "ResizeCompShareInstance",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			queried := wfCtx.Result("查询实例")
			args := map[string]any{
				"Region":  extractInstanceRegion(queried, defaultRegion),
				"Zone":    extractInstanceZone(queried, defaultZone),
				"UHostId": wfCtx.Params["UHostId"],
			}
			if cpu, ok := wfCtx.Params["Cpu"]; ok {
				args["Cpu"] = cpu
			}
			if gpu, ok := wfCtx.Params["Gpu"]; ok {
				args["Gpu"] = gpu
			}
			if mem, ok := wfCtx.Params["Memory"]; ok {
				args["Memory"] = mem
			}
			return args, nil
		},
	}
}
