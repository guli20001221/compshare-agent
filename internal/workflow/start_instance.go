package workflow

import "fmt"

const (
	// Upstream ResizeCompShareInstance accepts WithoutGpu=true with the fixed
	// 2C/4GB/GPU=0 shape. DescribeCompShareInstance.WithoutGpuSpec is not
	// returned for every normal GPU instance, so start flow must not require it.
	withoutGPUDefaultCPU    = float64(2)
	withoutGPUDefaultMemory = float64(4096)
	withoutGPUDefaultGPU    = float64(0)
)

// StartInstanceDef returns the workflow definition for starting a CompShare GPU
// instance. Normal start is query -> confirm -> start. Without-GPU start adds a
// resize-to-without-GPU step before the final start because the upstream start
// API does not accept a WithoutGpu parameter.
func StartInstanceDef() *Definition {
	return &Definition{
		Name:        "StartInstanceWorkflow",
		Description: "查询实例 → 确认开机 → 开机",
		Steps: []Step{
			stepQueryForStart(),
			stepConfirmStart(),
			stepResizeWithoutGPUForStart(),
			stepRestoreGPUForStart(),
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
			args := map[string]any{
				"UHostIds": []any{wfCtx.Params["UHostId"]},
			}
			return args, nil
		},
		CheckResult: func(wfCtx *Context, result map[string]any) (bool, string) {
			state := extractInstanceState(result)
			switch state {
			case "Stopped":
				if startWithoutGPURequested(wfCtx) {
					return validateWithoutGPUStart(result)
				}
				if restoreGPUStartRequired(result) {
					if _, ok := extractSourceGPUSpec(result); !ok {
						return false, "当前实例是无卡规格，未获取到原始带卡配置，无法安全带卡开机；如需继续无卡启动，请明确说“无卡启动”。"
					}
				}
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
			if startWithoutGPURequested(wfCtx) {
				summary["mode"] = "无卡模式（不分配 GPU，仅用于数据访问/维护）"
				if spec, ok := extractWithoutGPUSpec(wfCtx.Result("查询实例")); ok {
					summary["without_gpu_cpu"] = spec["Cpu"]
					summary["without_gpu_memory"] = spec["Memory"]
					summary["without_gpu_gpu"] = spec["Gpu"]
				}
			} else if spec, ok := extractSourceGPUSpec(wfCtx.Result("查询实例")); ok && restoreGPUStartRequired(wfCtx.Result("查询实例")) {
				summary["mode"] = "带卡模式（恢复原始 GPU 配置后开机）"
				summary["restore_cpu"] = spec["Cpu"]
				summary["restore_memory"] = spec["Memory"]
				summary["restore_gpu"] = spec["Gpu"]
			}
			return summary, nil
		},
	}
}

func stepResizeWithoutGPUForStart() Step {
	return Step{
		Name: "切换无卡规格",
		Type: StepToolCall,
		Tool: "ResizeCompShareInstance",
		SkipIf: func(wfCtx *Context) (bool, error) {
			return !startWithoutGPURequested(wfCtx), nil
		},
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			queried := wfCtx.Result("查询实例")
			spec, ok := extractWithoutGPUSpec(queried)
			if !ok {
				return nil, fmt.Errorf("未获取到无卡规格，无法安全切换无卡模式。")
			}
			region, zone, err := extractRequiredInstanceLocation(queried)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"Region":     region,
				"Zone":       zone,
				"UHostId":    wfCtx.Params["UHostId"],
				"WithoutGpu": true,
				"Cpu":        spec["Cpu"],
				"Memory":     spec["Memory"],
				"Gpu":        spec["Gpu"],
			}, nil
		},
	}
}

func stepRestoreGPUForStart() Step {
	return Step{
		Name: "恢复带卡规格",
		Type: StepToolCall,
		Tool: "ResizeCompShareInstance",
		SkipIf: func(wfCtx *Context) (bool, error) {
			if startWithoutGPURequested(wfCtx) {
				return true, nil
			}
			return !restoreGPUStartRequired(wfCtx.Result("查询实例")), nil
		},
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			queried := wfCtx.Result("查询实例")
			spec, ok := extractSourceGPUSpec(queried)
			if !ok {
				return nil, fmt.Errorf("未获取到原始带卡配置，无法安全恢复 GPU。")
			}
			region, zone, err := extractRequiredInstanceLocation(queried)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"Region":  region,
				"Zone":    zone,
				"UHostId": wfCtx.Params["UHostId"],
				"Cpu":     spec["Cpu"],
				"Memory":  spec["Memory"],
				"Gpu":     spec["Gpu"],
			}, nil
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
			region, zone, err := extractRequiredInstanceLocation(queried)
			if err != nil {
				return nil, err
			}
			args := map[string]any{
				"Region":  region,
				"Zone":    zone,
				"UHostId": wfCtx.Params["UHostId"],
			}
			return args, nil
		},
	}
}

func restoreGPUStartRequired(result map[string]any) bool {
	first := firstUHost(result)
	if first == nil {
		return false
	}
	if v, ok := first["GPU"]; ok {
		return anyFloat(v) == 0
	}
	if v, ok := first["Gpu"]; ok {
		return anyFloat(v) == 0
	}
	return false
}

func extractSourceGPUSpec(result map[string]any) (map[string]any, bool) {
	first := firstUHost(result)
	if first == nil {
		return nil, false
	}
	raw, ok := first["SrcInstanceConfig"].(map[string]any)
	if !ok || raw == nil {
		return nil, false
	}
	cpu := anyFloat(raw["Cpu"])
	mem := anyFloat(raw["Memory"])
	gpu := anyFloat(raw["Gpu"])
	if cpu <= 0 || mem <= 0 || gpu <= 0 {
		return nil, false
	}
	return map[string]any{
		"Cpu":    cpu,
		"Memory": mem,
		"Gpu":    gpu,
	}, true
}

func startWithoutGPURequested(wfCtx *Context) bool {
	if wfCtx == nil || wfCtx.Params == nil {
		return false
	}
	v, ok := wfCtx.Params["WithoutGpu"]
	if !ok {
		return false
	}
	switch typed := v.(type) {
	case bool:
		return typed
	case string:
		return typed == "true" || typed == "True" || typed == "TRUE"
	default:
		return false
	}
}

func validateWithoutGPUStart(result map[string]any) (bool, string) {
	if !extractFirstBool(result, "SupportWithoutGpuStart") {
		chargeType := extractField(result, "ChargeType")
		if chargeType != "" && chargeType != "Dynamic" && chargeType != "Postpay" {
			return false, "该实例当前计费形态不支持无卡开机。"
		}
		gpuType := extractField(result, "GpuType")
		if gpuType != "" {
			return false, fmt.Sprintf("该实例当前 GPU 型号 %s 不支持无卡开机。", gpuType)
		}
		return false, "该实例不支持无卡开机。"
	}
	return true, ""
}

func extractFirstBool(result map[string]any, key string) bool {
	first := firstUHost(result)
	if first == nil {
		return false
	}
	if v, ok := first[key].(bool); ok {
		return v
	}
	return false
}

func extractWithoutGPUSpec(result map[string]any) (map[string]any, bool) {
	first := firstUHost(result)
	if first == nil {
		return nil, false
	}
	raw, ok := first["WithoutGpuSpec"].(map[string]any)
	if !ok || raw == nil {
		if extractFirstBool(result, "SupportWithoutGpuStart") {
			return map[string]any{
				"Cpu":    withoutGPUDefaultCPU,
				"Memory": withoutGPUDefaultMemory,
				"Gpu":    withoutGPUDefaultGPU,
			}, true
		}
		return nil, false
	}
	spec := map[string]any{}
	for _, key := range []string{"Cpu", "Memory", "Gpu"} {
		v, ok := raw[key]
		if !ok {
			return nil, false
		}
		spec[key] = v
	}
	return spec, true
}

func firstUHost(result map[string]any) map[string]any {
	if result == nil {
		return nil
	}
	hostSet, ok := result["UHostSet"].([]any)
	if !ok || len(hostSet) == 0 {
		return nil
	}
	first, _ := hostSet[0].(map[string]any)
	return first
}
