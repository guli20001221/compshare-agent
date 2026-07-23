package workflow

import (
	"fmt"
	"strings"
)

const (
	// Upstream StartCompShareInstance takes WithoutGpuSpec ("A"=2C/4GB or
	// "B"=8C/16GB) directly and resizes internally before starting
	// (applyWithoutGpuBeforeStart). Both upstream tiers are exposed: A=2C/4GB,
	// B=8C/16GB. Pod instances support tier A only. The
	// older separate resize-then-start pattern (a raw WithoutGpu boolean sent
	// to ResizeCompShareInstance) is rejected outright by upstream now
	// (RejectDeprecatedResizeWithoutGpu) — do not reintroduce it.
	withoutGPUSpecA = "A"
	withoutGPUSpecB = "B"
)

// StartInstanceDef returns the workflow definition for starting a CompShare GPU
// instance. Normal start is query -> confirm -> start. Without-GPU start passes
// WithoutGpuSpec directly on the start call; upstream resizes internally before
// starting, so no separate client-side resize step is needed.
func StartInstanceDef() *Definition {
	return &Definition{
		Name: "StartInstanceWorkflow",
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
		CheckResult: func(wfCtx *Context, result map[string]any) CheckOutcome {
			state := extractInstanceState(result)
			switch state {
			case "Stopped":
				if spec := requestedWithoutGPUSpec(wfCtx); spec != "" {
					return validateWithoutGPUStart(result, spec)
				}
				return CheckPassed()
			case "":
				return CheckFailed("未找到该实例。")
			case "Running":
				return CheckFailed("实例当前已处于运行状态，无需重复开机。")
			case "Starting":
				return CheckFailed("实例正在启动中，请稍等。")
			default:
				return CheckFailed(fmt.Sprintf("实例当前状态为「%s」，仅 Stopped 状态可以开机。", state))
			}
		},
	}
}

func stepConfirmStart() Step {
	return Step{
		Name: "确认开机",
		Type: StepConfirm,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			if _, _, err := extractRequiredInstanceLocation(wfCtx.Result("查询实例"), nil); err != nil {
				return nil, err
			}
			summary := extractInstanceSummary(wfCtx.Result("查询实例"))
			if spec := requestedWithoutGPUSpec(wfCtx); spec != "" {
				cpu, memory, _ := withoutGPUSpecResources(spec)
				summary["mode"] = "无卡模式（不分配 GPU，仅用于数据访问/维护）"
				summary["without_gpu_spec"] = spec
				summary["without_gpu_cpu"] = cpu
				summary["without_gpu_memory"] = memory
				summary["without_gpu_gpu"] = float64(0)
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
			region, zone, err := extractRequiredInstanceLocation(queried, nil)
			if err != nil {
				return nil, err
			}
			args := map[string]any{
				"Region":  region,
				"Zone":    zone,
				"UHostId": wfCtx.Params["UHostId"],
			}
			if spec := requestedWithoutGPUSpec(wfCtx); spec != "" {
				args["WithoutGpuSpec"] = spec
			}
			return args, nil
		},
	}
}

func requestedWithoutGPUSpec(wfCtx *Context) string {
	if wfCtx == nil || wfCtx.Params == nil {
		return ""
	}
	v, ok := wfCtx.Params["WithoutGpuSpec"]
	if !ok {
		return ""
	}
	spec, _ := v.(string)
	return strings.TrimSpace(spec)
}

func withoutGPUSpecResources(spec string) (cpu, memory float64, ok bool) {
	switch spec {
	case withoutGPUSpecA:
		return 2, 4096, true
	case withoutGPUSpecB:
		return 8, 16384, true
	default:
		return 0, 0, false
	}
}

func validateWithoutGPUStart(result map[string]any, spec string) CheckOutcome {
	if _, _, ok := withoutGPUSpecResources(spec); !ok {
		return CheckFailed("无卡开机档位无效，仅支持 A（2C/4GB）或 B（8C/16GB）。")
	}
	if isPodInstanceResult(result) && spec != withoutGPUSpecA {
		return CheckFailed("容器实例的无卡开机仅支持 A 档（2C/4GB）。")
	}
	if !extractFirstBool(result, "SupportWithoutGpuStart") {
		chargeType := extractField(result, "ChargeType")
		if chargeType != "" && chargeType != "Dynamic" && chargeType != "Postpay" {
			return CheckFailed("该实例当前计费形态不支持无卡开机。")
		}
		gpuType := extractField(result, "GpuType")
		if gpuType != "" {
			return CheckFailed(fmt.Sprintf("该实例当前 GPU 型号 %s 不支持无卡开机。", gpuType))
		}
		return CheckFailed("该实例不支持无卡开机。")
	}
	return CheckPassed()
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
