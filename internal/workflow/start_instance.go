package workflow

import "fmt"

const (
	// Preview defaults for the confirm-card display when DescribeCompShareInstance
	// reports SupportWithoutGpuStart=true but doesn't return a WithoutGpuSpec
	// preview object. These mirror upstream spec A (2C4G) purely for display;
	// the actual upstream call always sends the "A"/"B" WithoutGpuSpec letter
	// directly to StartCompShareInstance (see withoutGPUSpecKey), never raw numbers.
	withoutGPUDefaultCPU    = float64(2)
	withoutGPUDefaultMemory = float64(4096)
	withoutGPUDefaultGPU    = float64(0)
)

// StartInstanceDef returns the workflow definition for starting a CompShare GPU
// instance. Normal start is query -> confirm -> start. Without-GPU start passes
// WithoutGpuSpec ("A" for Pod, "B" for UCloud) directly on the StartCompShareInstance
// call itself — upstream removed the old two-step resize-then-start contract and
// now hard-rejects any request that still carries the deprecated WithoutGpu boolean.
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
			region, zone, err := extractRequiredInstanceLocation(queried)
			if err != nil {
				return nil, err
			}
			args := map[string]any{
				"Region":  region,
				"Zone":    zone,
				"UHostId": wfCtx.Params["UHostId"],
			}
			if startWithoutGPURequested(wfCtx) {
				args["WithoutGpuSpec"] = withoutGPUSpecKey(queried)
			}
			return args, nil
		},
	}
}

// withoutGPUSpecKey returns the upstream WithoutGpuSpec letter for this
// instance. Pod (Container) instances only support spec A (2C4G); UCloud
// instances use spec B (8C16G).
func withoutGPUSpecKey(queried map[string]any) string {
	if extractInstanceType(queried) == "Container" {
		return "A"
	}
	return "B"
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
