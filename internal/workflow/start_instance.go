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
	//
	// "Resizes internally before starting" is the whole hazard, so spell it out:
	// upstream commits the resize as its own step (saving the original spec to
	// the instance's SrcInstanceConfig label) and only then boots. The resize is
	// NOT rolled back when the boot fails, and it is not a mode the instance
	// leaves by being stopped — the instance's configured spec is now 0 GPU, and
	// getting the original back needs the target zone to have that GPU available
	// again. A parameter that reads like a boot flag is therefore an irreversible
	// spec change wearing a boot flag's name — which is why its schema description
	// says so in those terms, and why the confirmation card below states the whole
	// before→after instead of describing the instance as it currently is.
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
			queried := wfCtx.Result("查询实例")
			if _, _, err := extractRequiredInstanceLocation(queried, nil); err != nil {
				return nil, err
			}
			summary := extractInstanceSummary(queried)
			if spec := requestedWithoutGPUSpec(wfCtx); spec != "" {
				// extractInstanceSummary describes the instance as it is NOW, and the
				// console gives GpuType/GPU a labelled row of their own. On a no-GPU
				// start that row describes the spec this card is about to take away,
				// while the four raw without_gpu_* keys that described the replacement
				// had no label at all — so the card's loudest statement was
				// "GPU 3090 × 1" on the very card that removed the 3090.
				//
				// Say the change as one before→after sentence the console cannot
				// mislabel, and drop the current-spec rows it contradicts.
				cpu, memory, _ := withoutGPUSpecResources(spec)
				summary["规格变更"] = fmt.Sprintf("%s → 无卡（0 GPU / %.0f核 / %.0fGB）",
					currentSpecLabel(queried), cpu, memory/1024)
				summary["注意"] = "这不是普通开机：实例会先被改配为无卡规格再启动，原规格需要该可用区有可用 GPU 时才能恢复。"
				delete(summary, "GpuType")
				delete(summary, "GPU")
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

// currentSpecLabel renders the spec the instance has right now, for the left side
// of the card's before→after line. It reports only what the Describe response
// actually carried: a field the response omitted is left out rather than printed
// as zero, because "0 GPU" on the BEFORE side of a card that removes the GPU would
// be a lie in the one direction that matters.
func currentSpecLabel(result map[string]any) string {
	host := firstUHost(result)
	if host == nil {
		return "当前规格"
	}
	parts := make([]string, 0, 3)
	gpuType, _ := host["GpuType"].(string)
	gpu := firstNumberField(host, "GPU", "Gpu")
	switch {
	case gpuType != "" && gpu > 0:
		parts = append(parts, fmt.Sprintf("%s × %.0f", gpuType, gpu))
	case gpuType != "":
		parts = append(parts, gpuType)
	case gpu > 0:
		parts = append(parts, fmt.Sprintf("%.0f 卡", gpu))
	}
	if cpu := firstNumberField(host, "CPU", "Cpu"); cpu > 0 {
		parts = append(parts, fmt.Sprintf("%.0f核", cpu))
	}
	if memory := firstNumberField(host, "Memory", "Mem"); memory > 0 {
		parts = append(parts, fmt.Sprintf("%.0fGB", memory/1024))
	}
	if len(parts) == 0 {
		return "当前规格"
	}
	return strings.Join(parts, " / ")
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
