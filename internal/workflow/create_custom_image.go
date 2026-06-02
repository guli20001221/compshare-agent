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
		CheckResult: func(_ *Context, result map[string]any) (bool, string) {
			state := extractInstanceState(result)
			switch state {
			case "Running", "Stopped":
				return true, ""
			case "":
				return false, "未找到源实例，无法创建自制镜像。"
			default:
				return false, fmt.Sprintf("源实例当前状态为 %s，暂不能创建自制镜像。", state)
			}
		},
	}
}

func stepConfirmCreateCustomImage() Step {
	return Step{
		Name: "确认创建自制镜像",
		Type: StepConfirm,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			name := paramStr(wfCtx.Params, "Name", "")
			if name == "" {
				return nil, fmt.Errorf("image Name is required; ask the user for a custom image name before creating it")
			}
			summary := extractInstanceSummary(wfCtx.Result("查询源实例"))
			summary["workflow"] = "CreateCustomImageWorkflow"
			summary["UHostId"] = paramStr(wfCtx.Params, "UHostId", "")
			summary["Name"] = name
			if description := paramStr(wfCtx.Params, "Description", ""); description != "" {
				summary["Description"] = description
			}
			summary["warning"] = "将基于该实例创建自制镜像。虚机 Running/Stopped 均可制作；容器实例通常需要 Running，若平台返回限制错误，请先启动实例后重试。"
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
				return nil, fmt.Errorf("image Name is required before creating a custom image")
			}
			args := map[string]any{
				"UHostId": uHostId,
				"Name":    name,
			}
			if description := paramStr(wfCtx.Params, "Description", ""); description != "" {
				args["Description"] = description
			}
			return args, nil
		},
		CheckResult: func(_ *Context, result map[string]any) (bool, string) {
			if customImageID(result) == "" {
				return false, "创建自制镜像未返回 CompShareImageId。"
			}
			return true, ""
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
			return map[string]any{
				"CompShareImageId": imageID,
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
