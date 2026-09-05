package workflow

import (
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/deployment"
)

const resizeInstanceMissingSpecMessage = "变配请求必须至少指定 Cpu、Gpu、Memory 之一"

const (
	resizeResolvedCPUKey    = "ResolvedResizeCPU"
	resizeResolvedGPUKey    = "ResolvedResizeGPU"
	resizeResolvedMemoryKey = "ResolvedResizeMemory"
)

func ResizeInstanceDef() *Definition {
	return &Definition{
		Name: "ResizeInstanceWorkflow",
		Steps: []Step{
			stepQueryForResize(),
			stepQuerySupportZones(),
			stepQueryResizeAvailableSpecs(),
			stepCheckResizeCapacity(),
			stepQueryResizePrice(),
			stepConfirmResize(),
			stepResizeInstance(),
		},
	}
}

func stepCheckResizeCapacity() Step {
	return Step{
		Name: "检查目标规格库存",
		Type: StepToolCall,
		Tool: "CheckCompShareResourceCapacity",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			return resizeCapacityArgs(wfCtx)
		},
		CheckResult: func(wfCtx *Context, result map[string]any) CheckOutcome {
			cpu, gpu, memory, err := resolvedResizeTarget(wfCtx)
			if err != nil {
				return CheckFailed(err.Error())
			}
			targetCPU, cpuOK := capacityInt(cpu)
			targetGPU, gpuOK := capacityInt(gpu)
			targetMemoryMB, memoryOK := capacityInt(memory)
			if !cpuOK || !gpuOK || !memoryOK || targetCPU <= 0 || targetGPU <= 0 || targetMemoryMB <= 0 || targetMemoryMB%1024 != 0 {
				return CheckFailed("未解析出完整的目标变配规格，无法确认实时容量。")
			}

			specs := parseCapacitySpecs(result)
			if !capacityHasSignal(specs) {
				return CheckFailed("容量检查未返回可用规格，无法确认目标配置当前是否可变配。")
			}
			for _, spec := range specs {
				if spec.GPU != targetGPU || spec.CPU != targetCPU || spec.MemGB != targetMemoryMB/1024 {
					continue
				}
				if !spec.Enough {
					return CheckFailedBecause(ReasonCapacitySoldOut,
						fmt.Sprintf("%d GPU / %dC / %dGB 的目标配置当前库存不足。", targetGPU, targetCPU, targetMemoryMB/1024))
				}
				return CheckPassed()
			}
			return CheckFailed("容量检查未返回目标变配规格，无法确认该配置当前是否可变配。")
		},
	}
}

func resizeCapacityArgs(wfCtx *Context) (map[string]any, error) {
	queried := wfCtx.Result("查询实例")
	host := firstUHost(queried)
	if host == nil {
		return nil, fmt.Errorf("未找到该实例。")
	}

	uhostID := strings.TrimSpace(stringFieldAny(host["UHostId"]))
	gpuType := strings.TrimSpace(stringFieldAny(host["GpuType"]))
	machineType := strings.TrimSpace(stringFieldAny(host["MachineType"]))
	cpuPlatform := strings.TrimSpace(stringFieldAny(host["CpuPlatform"]))
	imageID := strings.TrimSpace(stringFieldAny(host["CompShareImageId"]))
	chargeType := strings.TrimSpace(stringFieldAny(host["ChargeType"]))
	if uhostID == "" || gpuType == "" || machineType == "" || cpuPlatform == "" || imageID == "" || chargeType == "" {
		return nil, fmt.Errorf("实例详情缺少容量检查所需的机型、镜像或计费信息，无法确认目标配置当前是否可变配。")
	}
	if paramBool(host, "IsSpot", false) {
		chargeType = deployment.ChargeTypeSpot
	}

	disks, err := resizeCapacityDisks(host)
	if err != nil {
		return nil, err
	}
	region, zone, err := extractRequiredInstanceLocation(queried, wfCtx.Result("查询支持区"))
	if err != nil {
		return nil, err
	}
	isPod := isPodInstanceResult(queried)
	placement := deployment.ZonePlacement{
		Zone:   zone,
		Region: region,
		IsPod:  isPod,
	}
	if observed, ok := supportZonePlacementForZone(wfCtx.Result("查询支持区"), zone); ok {
		placement.ZoneID = observed.zoneID
		placement.AzGroup = observed.azGroup
	}
	if isPod && placement.ZoneID == 0 {
		return nil, fmt.Errorf("实时可用区目录未返回实例所在区的内部编号，无法确认目标配置当前是否可变配。")
	}

	args := deployment.BuildCapacityArgs(deployment.DeploymentDraft{
		Zone:               zone,
		GPUType:            gpuType,
		CompShareImageID:   imageID,
		ChargeType:         chargeType,
		Disks:              disks,
		MinimalCPUPlatform: cpuPlatform,
	})
	// Capacity for an existing instance must use its observed billing pool;
	// creation-only normalization would turn Dynamic into Postpay.
	args["ChargeType"] = chargeType
	args["UHostId"] = uhostID
	args["MachineType"] = machineType
	return deployment.ApplyCapacityPlacementArgs(args, placement), nil
}

func resizeCapacityDisks(host map[string]any) ([]any, error) {
	rawDisks, ok := host["DiskSet"].([]any)
	if !ok || len(rawDisks) == 0 {
		return nil, fmt.Errorf("实例详情未返回磁盘配置，无法确认目标配置当前是否可变配。")
	}
	disks := make([]any, 0, len(rawDisks))
	for _, raw := range rawDisks {
		disk, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("实例磁盘配置格式不完整，无法确认目标配置当前是否可变配。")
		}
		diskType := strings.TrimSpace(stringFieldAny(disk["DiskType"]))
		size, sizeOK := parseUint32Any(disk["Size"])
		if diskType == "" || !sizeOK || size == 0 {
			return nil, fmt.Errorf("实例磁盘配置缺少类型或容量，无法确认目标配置当前是否可变配。")
		}
		isBoot := paramBool(disk, "IsBoot", false) || strings.EqualFold(strings.TrimSpace(stringFieldAny(disk["Type"])), "Boot")
		disks = append(disks, map[string]any{
			"IsBoot": isBoot,
			"Type":   diskType,
			"Size":   size,
		})
	}
	return disks, nil
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
				return nil, NewMissingSlotError(resizeInstanceMissingSpecMessage, "cpu", "memory_gb", "gpu_count")
			}
			return map[string]any{
				"UHostIds": []any{wfCtx.Params["UHostId"]},
			}, nil
		},
		CheckResult: func(wfCtx *Context, result map[string]any) CheckOutcome {
			state := extractInstanceState(result)
			switch state {
			case "Stopped":
				if !resizeHasEffectiveSpecChange(wfCtx.Params, result) {
					return CheckFailed("目标配置与当前配置一致，无需变配。")
				}
				if host := firstUHost(result); host != nil {
					wfCtx.Params["ResizeInitialCPU"] = host["CPU"]
					wfCtx.Params["ResizeInitialGPU"] = host["GPU"]
					wfCtx.Params["ResizeInitialMemory"] = host["Memory"]
				}
				return CheckPassed()
			case "":
				return CheckFailed("未找到该实例。")
			case "Running":
				return CheckFailed("实例当前正在运行，变配需要先关机。")
			case "Stopping":
				return CheckFailed("实例正在关机中，请稍后再试。")
			default:
				return CheckFailed(fmt.Sprintf("实例当前状态为「%s」，仅 Stopped 状态可以变配。", state))
			}
		},
	}
}

func stepQueryResizeAvailableSpecs() Step {
	return Step{
		Name: "查询合法规格",
		Type: StepToolCall,
		Tool: "DescribeAvailableCompShareInstanceTypes",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			queried := wfCtx.Result("查询实例")
			region, zone, err := extractRequiredInstanceLocation(queried, wfCtx.Result("查询支持区"))
			if err != nil {
				return nil, err
			}
			args := map[string]any{
				"Region": region,
				"Zone":   zone,
			}
			if gpuType := resizeCurrentGPUType(queried); gpuType != "" {
				args["MachineTypes"] = []any{gpuType}
			}
			return args, nil
		},
		CheckResult: func(wfCtx *Context, result map[string]any) CheckOutcome {
			return validateResizeTargetSpec(wfCtx, result)
		},
	}
}

func resizeHasEffectiveSpecChange(params map[string]any, result map[string]any) bool {
	hostSet, ok := result["UHostSet"].([]any)
	if !ok || len(hostSet) == 0 {
		return true
	}
	host, ok := hostSet[0].(map[string]any)
	if !ok {
		return true
	}
	checks := []struct {
		paramKey string
		hostKey  string
	}{
		{paramKey: "Cpu", hostKey: "CPU"},
		{paramKey: "Gpu", hostKey: "GPU"},
		{paramKey: "Memory", hostKey: "Memory"},
	}
	for _, check := range checks {
		target, hasTarget := priceNumber(params[check.paramKey])
		if !hasTarget {
			continue
		}
		current, hasCurrent := priceNumber(host[check.hostKey])
		if !hasCurrent {
			return true
		}
		if target != current {
			return true
		}
	}
	return false
}

func validateResizeTargetSpec(wfCtx *Context, catalog map[string]any) CheckOutcome {
	queried := wfCtx.Result("查询实例")
	host := firstUHost(queried)
	if host == nil {
		return CheckFailed("未找到该实例。")
	}
	gpuType, _ := host["GpuType"].(string)
	if gpuType == "" {
		return CheckFailed("未获取到实例 GPU 型号，无法确认合法变配规格。")
	}
	zone, _ := host["Zone"].(string)
	targetGPU := resizeTargetNumber(wfCtx.Params, host, "Gpu", "GPU")
	targetCPU := resizeTargetNumber(wfCtx.Params, host, "Cpu", "CPU")
	targetMemory := resizeTargetNumber(wfCtx.Params, host, "Memory", "Memory")
	if targetGPU == 0 || targetCPU == 0 || targetMemory == 0 {
		return CheckFailed("未获取到完整的目标 CPU/GPU/内存配置，无法确认合法变配规格。")
	}
	candidates := listSpecCandidates(catalog, gpuType, targetGPU, zone)
	if len(candidates) == 0 {
		return CheckFailed(fmt.Sprintf("未找到 %s × %.0f 卡的合法变配规格，请稍后重试或到控制台确认。", gpuType, targetGPU))
	}
	for _, candidate := range candidates {
		if candidate.CPU == targetCPU && candidate.MemoryMB == targetMemory {
			// Seal one complete target tuple after the live catalog accepted it.
			// Pod resize requires Cpu and Memory even when the user changed only
			// one dimension; price, confirmation and mutation must therefore all
			// consume this exact materialized target rather than the sparse model
			// proposal.
			wfCtx.Params[resizeResolvedCPUKey] = targetCPU
			wfCtx.Params[resizeResolvedGPUKey] = targetGPU
			wfCtx.Params[resizeResolvedMemoryKey] = targetMemory
			return CheckPassed()
		}
	}
	return CheckFailed(fmt.Sprintf("%s × %.0f 卡不支持 %.0fC/%.0fGB 的变配目标。合法选项：%s",
		gpuType, targetGPU, targetCPU, targetMemory/1024, formatCandidates(candidates)))
}

func resizeTargetNumber(params map[string]any, host map[string]any, paramKey, hostKey string) float64 {
	if v, ok := params[paramKey]; ok {
		return paramNum(map[string]any{paramKey: v}, paramKey, 0)
	}
	value, _ := priceNumber(host[hostKey])
	return value
}

func resizeCurrentGPUType(result map[string]any) string {
	host := firstUHost(result)
	if host == nil {
		return ""
	}
	value, _ := host["GpuType"].(string)
	return value
}

func resolvedResizeTarget(wfCtx *Context) (cpu, gpu, memory any, err error) {
	if wfCtx == nil || wfCtx.Params == nil {
		return nil, nil, nil, fmt.Errorf("未解析出完整的目标变配规格。")
	}
	cpu, cpuOK := wfCtx.Params[resizeResolvedCPUKey]
	gpu, gpuOK := wfCtx.Params[resizeResolvedGPUKey]
	memory, memoryOK := wfCtx.Params[resizeResolvedMemoryKey]
	if !cpuOK || !gpuOK || !memoryOK {
		return nil, nil, nil, fmt.Errorf("未解析出完整的目标变配规格。")
	}
	return cpu, gpu, memory, nil
}

func stepQueryResizePrice() Step {
	return Step{
		Name: "查询变配价格",
		Type: StepToolCall,
		Tool: "GetCompShareInstanceUpgradePrice",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			queried := wfCtx.Result("查询实例")
			cpu, gpu, memory, err := resolvedResizeTarget(wfCtx)
			if err != nil {
				return nil, err
			}
			args := map[string]any{
				"UHostId": wfCtx.Params["UHostId"],
				"CPU":     cpu,
				"GPU":     gpu,
				"Memory":  memory,
			}
			if _, err := addRequiredPodPlacementArgs(args, queried, wfCtx.Result("查询支持区")); err != nil {
				return nil, err
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
			cpu, gpu, memory, err := resolvedResizeTarget(wfCtx)
			if err != nil {
				return nil, err
			}
			summary := extractInstanceSummary(wfCtx.Result("查询实例"))
			summary["target_cpu"] = cpu
			summary["target_gpu"] = gpu
			summary["target_memory"] = memory
			priceResult := wfCtx.Result("查询变配价格")
			price, err := requiredPriceField(priceResult, "Price")
			if err != nil {
				return nil, err
			}
			summary["price_delta"] = price
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
			cpu, gpu, memory, err := resolvedResizeTarget(wfCtx)
			if err != nil {
				return nil, err
			}
			args := map[string]any{
				"UHostId": wfCtx.Params["UHostId"],
				"Cpu":     cpu,
				"Gpu":     gpu,
				"Memory":  memory,
			}
			if _, err := addRequiredPodPlacementArgs(args, queried, wfCtx.Result("查询支持区")); err != nil {
				return nil, err
			}
			return args, nil
		},
	}
}
