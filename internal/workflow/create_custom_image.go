package workflow

import (
	"fmt"
	"time"
)

func CreateCustomImageDef() *Definition {
	return &Definition{
		Name: "CreateCustomImageWorkflow",
		Steps: []Step{
			stepQuerySourceInstanceForCustomImage(),
			stepQuerySupportZonesForCustomImage(),
			stepConfirmCreateCustomImage(),
			stepStopSourceForCustomImage(),
			stepWaitSourceStoppedForCustomImage(),
			stepCreateCustomImage(),
			stepGetCustomImageCreateProgress(),
		},
		ResultData: customImageResultData,
	}
}

func stepQuerySupportZonesForCustomImage() Step {
	return Step{
		Name: "查询源实例可用区",
		Type: StepToolCall,
		Tool: "DescribeCompShareSupportZone",
		BuildArgs: func(_ *Context) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
}

func stepQuerySourceInstanceForCustomImage() Step {
	return Step{
		Name: "查询源实例",
		Type: StepToolCall,
		Tool: "DescribeCompShareInstance",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			uHostId := paramStr(wfCtx.Params, "UHostId", "")
			if uHostId == "" {
				return nil, fmt.Errorf("UHostId is required before creating a custom image")
			}
			return map[string]any{
				"UHostIds": []any{uHostId},
			}, nil
		},
		CheckResult: func(_ *Context, result map[string]any) CheckOutcome {
			state := extractInstanceState(result)
			switch state {
			case "Running", "Stopped":
				if _, _, ok := sourceCustomImagePlacement(result); !ok {
					return CheckFailed("源实例缺少可用区或地域信息，无法安全创建自制镜像。")
				}
				if sourceCustomImageUnsupportedWithoutGPU(result) {
					return CheckFailed("该虚机当前处于 2C/4GB 无卡模式；上游仅允许 8C/16GB 无卡虚机制作自制镜像。请先恢复有卡配置或切换到 8C/16GB 无卡档位。")
				}
				if sourceCustomImageRequiresRunning(result) && state != "Running" {
					return CheckFailed("容器来源实例创建自制镜像需要先开机，请启动实例后再创建。")
				}
				return CheckPassed()
			case "":
				return CheckFailed("未找到源实例，无法创建自制镜像。")
			default:
				return CheckFailed(fmt.Sprintf("源实例当前状态为 %s，暂不能创建自制镜像。", state))
			}
		},
	}
}

func sourceCustomImageUnsupportedWithoutGPU(result map[string]any) bool {
	if isContainerInstanceResult(result) {
		return false
	}
	host := firstUHost(result)
	if host == nil {
		return false
	}
	if spec, ok := host["WithoutGpuSpec"].(map[string]any); ok {
		if name, _ := spec["Spec"].(string); name != "" {
			return name == "A"
		}
	}
	return paramNum(host, "GPU", -1) == 0 &&
		paramNum(host, "CPU", -1) == 2 &&
		paramNum(host, "Memory", -1) == 4096
}

func stepConfirmCreateCustomImage() Step {
	return Step{
		Name: "确认创建自制镜像",
		Type: StepConfirm,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			name := paramStr(wfCtx.Params, "Name", "")
			if name == "" {
				return nil, NewMissingSlotError("创建自制镜像需要指定名称。", "name")
			}
			summary := extractInstanceSummary(wfCtx.Result("查询源实例"))
			summary["workflow"] = "CreateCustomImageWorkflow"
			summary["UHostId"] = paramStr(wfCtx.Params, "UHostId", "")
			summary["Name"] = name
			region, zone, ok := sourceCustomImagePlacement(wfCtx.Result("查询源实例"))
			if !ok {
				return nil, fmt.Errorf("源实例缺少可用区或地域信息，无法安全创建自制镜像")
			}
			summary["Region"] = region
			summary["Zone"] = zone
			if description := paramStr(wfCtx.Params, "Description", ""); description != "" {
				summary["Description"] = description
			}
			if sourceCustomImageNeedsStop(wfCtx.Result("查询源实例")) {
				summary["warning"] = "将先关闭这台普通虚机，待关机完成后创建自制镜像；制作完成后实例保持关机。"
			} else {
				summary["warning"] = "将基于该实例创建自制镜像。容器来源实例会保持运行。"
			}
			return summary, nil
		},
	}
}

func stepStopSourceForCustomImage() Step {
	return Step{
		Name: "关闭源实例",
		Type: StepToolCall,
		Tool: "StopCompShareInstance",
		SkipIf: func(wfCtx *Context) (bool, error) {
			return !sourceCustomImageNeedsStop(wfCtx.Result("查询源实例")), nil
		},
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			args := map[string]any{"UHostId": paramStr(wfCtx.Params, "UHostId", "")}
			return addCustomImagePlacementArgs(args, wfCtx)
		},
	}
}

func stepWaitSourceStoppedForCustomImage() Step {
	return Step{
		Name: "等待源实例关机",
		Type: StepToolCall,
		Tool: "DescribeCompShareInstance",
		SkipIf: func(wfCtx *Context) (bool, error) {
			return !sourceCustomImageNeedsStop(wfCtx.Result("查询源实例")), nil
		},
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			return map[string]any{"UHostIds": []any{paramStr(wfCtx.Params, "UHostId", "")}}, nil
		},
		CheckResult: func(_ *Context, result map[string]any) CheckOutcome {
			switch state := extractInstanceState(result); state {
			case "Stopped":
				return CheckPassed()
			case "Running", "Stopping":
				return CheckPending("已发起关机，但实例在等待时限内仍未停止；未创建镜像，请稍后重试。")
			case "":
				return CheckFailed("关机后未能重新查询到源实例；未创建镜像。")
			default:
				return CheckFailed(fmt.Sprintf("源实例关机后进入了状态 %s；未创建镜像。", state))
			}
		},
		Poll: &PollPolicy{Interval: 2 * time.Second, Timeout: 2 * time.Minute},
	}
}

func stepCreateCustomImage() Step {
	return Step{
		Name: "创建自制镜像",
		Type: StepToolCall,
		Tool: "CreateCompShareCustomImage",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			uHostId := paramStr(wfCtx.Params, "UHostId", "")
			if uHostId == "" {
				return nil, fmt.Errorf("UHostId is required before creating a custom image")
			}
			name := paramStr(wfCtx.Params, "Name", "")
			if name == "" {
				return nil, NewMissingSlotError("创建自制镜像需要指定名称。", "name")
			}
			args := map[string]any{
				"UHostId": uHostId,
				"Name":    name,
			}
			if description := paramStr(wfCtx.Params, "Description", ""); description != "" {
				args["Description"] = description
			}
			return addCustomImagePlacementArgs(args, wfCtx)
		},
		CheckResult: func(_ *Context, result map[string]any) CheckOutcome {
			if customImageID(result) == "" {
				return CheckFailed("创建自制镜像未返回 CompShareImageId。")
			}
			return CheckPassed()
		},
	}
}

func stepGetCustomImageCreateProgress() Step {
	return Step{
		Name:     "查询镜像制作进度",
		Type:     StepToolCall,
		Tool:     "GetCompShareImageCreateProgress",
		Optional: true,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			imageID := customImageID(wfCtx.Result("创建自制镜像"))
			if imageID == "" {
				return nil, fmt.Errorf("CompShareImageId is empty; skip progress query")
			}
			return addCustomImagePlacementArgs(map[string]any{
				"CompShareImageId": imageID,
			}, wfCtx)
		},
	}
}

func customImageResultData(wfCtx *Context) map[string]any {
	out := map[string]any{}
	imageID := customImageID(wfCtx.Result("创建自制镜像"))
	if imageID != "" {
		out["CompShareImageId"] = imageID
	}
	if progress := wfCtx.Result("查询镜像制作进度"); len(progress) > 0 {
		out["Progress"] = progress
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func customImageID(result map[string]any) string {
	if result == nil {
		return ""
	}
	if id, ok := result["CompShareImageId"].(string); ok {
		return id
	}
	return ""
}

func sourceCustomImagePlacement(result map[string]any) (region, zone string, ok bool) {
	zone = extractInstanceZone(result, "")
	region = extractInstanceRegion(result, "")
	return region, zone, region != "" && zone != ""
}

func sourceCustomImageRequiresRunning(result map[string]any) bool {
	return isPodInstanceResult(result) || isContainerInstanceResult(result)
}

func sourceCustomImageNeedsStop(result map[string]any) bool {
	return !sourceCustomImageRequiresRunning(result) && extractInstanceState(result) == "Running"
}

func addCustomImagePlacementArgs(args map[string]any, wfCtx *Context) (map[string]any, error) {
	region, zone, ok := sourceCustomImagePlacement(wfCtx.Result("查询源实例"))
	if !ok {
		return nil, fmt.Errorf("源实例缺少可用区或地域信息，无法安全创建自制镜像")
	}
	placement, ok := supportZonePlacementForZone(wfCtx.Result("查询源实例可用区"), zone)
	if !ok || placement.azGroup == 0 {
		return nil, fmt.Errorf("未获取到源实例内部可用区编号，无法安全创建自制镜像")
	}
	args["Region"] = region
	args["Zone"] = zone
	args["az_group"] = placement.azGroup
	if placement.zoneID != 0 {
		args["zone_id"] = placement.zoneID
	}
	return args, nil
}
