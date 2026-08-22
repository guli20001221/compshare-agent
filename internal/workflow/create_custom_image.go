package workflow

import (
	"fmt"
)

// CustomImageSourceInstanceNote states what制作 does to the SOURCE instance.
//
// "不会关闭源实例" is true, and was itself the fix for an earlier reply that
// wrongly promised a shutdown. But alone it reads as "nothing changes for this
// machine", which is not what happens: upstream holds the instance in ImageMaking
// for the duration, releasing its public address and rejecting 开关机 / 重装系统 /
// 变更配置 with 8964 until制作 ends. A user told only that the machine stays up
// then loses SSH and reads it as a fault.
//
// Exported and shared with the engine's post-write reply (customImageWorkflowReply)
// so the confirmation card and the confirmation of what happened cannot drift into
// telling a user two different things about the same operation.
const CustomImageSourceInstanceNote = "制作期间源实例会进入 ImageMaking 状态：公网地址会被释放，开关机、重装系统、变更配置都会被拒绝，需等制作结束后恢复。"

func CreateCustomImageDef() *Definition {
	return &Definition{
		Name: "CreateCustomImageWorkflow",
		Steps: []Step{
			stepQuerySourceInstanceForCustomImage(),
			stepQuerySupportZonesForCustomImage(),
			stepConfirmCreateCustomImage(),
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
				if extractInstanceZone(result, "") == "" {
					return CheckFailed("源实例缺少可用区信息，无法安全创建自制镜像。")
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
			name, err := validatedCompShareResourceName(name, "自制镜像名称", 50)
			if err != nil {
				return nil, err
			}
			summary := extractInstanceSummary(wfCtx.Result("查询源实例"))
			summary["workflow"] = "CreateCustomImageWorkflow"
			summary["UHostId"] = paramStr(wfCtx.Params, "UHostId", "")
			summary["Name"] = name
			region, zone, ok := sourceCustomImagePlacement(
				wfCtx.Result("查询源实例"), wfCtx.Result("查询源实例可用区"))
			if !ok {
				return nil, fmt.Errorf("源实例缺少可用区或地域信息，无法安全创建自制镜像")
			}
			summary["Region"] = region
			summary["Zone"] = zone
			if description := paramStr(wfCtx.Params, "Description", ""); description != "" {
				summary["Description"] = description
			}
			summary["warning"] = "将基于该实例发起自制镜像制作，不会关闭源实例。但" +
				CustomImageSourceInstanceNote +
				"镜像初始状态为 Making，变为 Available 后才能用于创建实例、共享或克隆。"
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
			name, err := validatedCompShareResourceName(name, "自制镜像名称", 50)
			if err != nil {
				return nil, err
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
		Name: "查询镜像制作状态",
		Type: StepToolCall,
		ToolFunc: func(wfCtx *Context) string {
			// Pod custom images are backed by UHub.  Although the platform routes
			// GetCompShareImageCreateProgress through the shared image handler, that
			// handler explicitly rejects UHub-backed images.  Read the just-created
			// catalog row instead; VM images keep the percentage-progress API.
			if isPodInstanceResult(wfCtx.Result("查询源实例")) {
				return "DescribeCompShareCustomImages"
			}
			return "GetCompShareImageCreateProgress"
		},
		Optional: true,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			imageID := customImageID(wfCtx.Result("创建自制镜像"))
			if imageID == "" {
				return nil, fmt.Errorf("CompShareImageId is empty; skip creation-status query")
			}
			if isPodInstanceResult(wfCtx.Result("查询源实例")) {
				return map[string]any{
					"CompShareImageId": imageID,
					"Limit":            1,
				}, nil
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
	if readback := wfCtx.Result("查询镜像制作状态"); len(readback) > 0 {
		if isPodInstanceResult(wfCtx.Result("查询源实例")) {
			if status := customImageCatalogStatus(readback, imageID); status != "" {
				out["Status"] = status
			}
		} else {
			out["Progress"] = readback
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func customImageCatalogStatus(result map[string]any, imageID string) string {
	rows, _ := result["ImageSet"].([]any)
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, _ := row["CompShareImageId"].(string)
		if id != imageID {
			continue
		}
		status, _ := row["Status"].(string)
		return status
	}
	return ""
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

func sourceCustomImagePlacement(result, supportZones map[string]any) (region, zone string, ok bool) {
	region, zone, err := extractRequiredInstanceLocation(result, supportZones)
	return region, zone, err == nil
}

func sourceCustomImageRequiresRunning(result map[string]any) bool {
	return isPodInstanceResult(result) || isContainerInstanceResult(result)
}

func addCustomImagePlacementArgs(args map[string]any, wfCtx *Context) (map[string]any, error) {
	region, zone, ok := sourceCustomImagePlacement(
		wfCtx.Result("查询源实例"), wfCtx.Result("查询源实例可用区"))
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
