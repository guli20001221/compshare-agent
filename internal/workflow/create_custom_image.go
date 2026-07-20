package workflow

import "fmt"

func CreateCustomImageDef() *Definition {
	return &Definition{
		Name:        "CreateCustomImageWorkflow",
		Description: "查询源实例 -> 确认创建自制镜像 -> 创建镜像 -> 查询制作进度",
		Steps: []Step{
			stepQuerySourceInstanceForCustomImage(),
			stepConfirmCreateCustomImage(),
			stepCreateCustomImage(),
			stepGetCustomImageCreateProgress(),
		},
		ResultData: customImageResultData,
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
			summary["warning"] = "将基于该实例创建自制镜像。虚机 Running/Stopped 均可制作；容器实例需要 Running。"
			return summary, nil
		},
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
			region, zone, ok := sourceCustomImagePlacement(wfCtx.Result("查询源实例"))
			if !ok {
				return nil, fmt.Errorf("源实例缺少可用区或地域信息，无法安全创建自制镜像")
			}
			args := map[string]any{
				"UHostId": uHostId,
				"Name":    name,
				"Region":  region,
				"Zone":    zone,
			}
			if description := paramStr(wfCtx.Params, "Description", ""); description != "" {
				args["Description"] = description
			}
			return args, nil
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
			region, zone, ok := sourceCustomImagePlacement(wfCtx.Result("查询源实例"))
			if !ok {
				return nil, fmt.Errorf("源实例缺少可用区或地域信息，无法查询镜像制作进度")
			}
			return map[string]any{
				"CompShareImageId": imageID,
				"Region":           region,
				"Zone":             zone,
			}, nil
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
