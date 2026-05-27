package workflow

import (
	"encoding/base64"
	"fmt"
)

func ReinstallInstanceDef() *Definition {
	return &Definition{
		Name:        "ReinstallInstanceWorkflow",
		Description: "查询实例 → 查询目标镜像 → 确认重装 → 重装系统",
		Steps: []Step{
			stepQueryForReinstall(),
			stepQueryTargetImage(),
			stepConfirmReinstall(),
			stepReinstallInstance(),
		},
	}
}

func stepQueryForReinstall() Step {
	return Step{
		Name: "查询实例",
		Type: StepToolCall,
		Tool: "DescribeCompShareInstance",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
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
				return false, "实例当前正在运行，重装系统需要先关机。"
			case "Stopping":
				return false, "实例正在关机中，请稍后再试。"
			default:
				return false, fmt.Sprintf("实例当前状态为「%s」，仅 Stopped 状态可以重装。", state)
			}
		},
	}
}

func stepQueryTargetImage() Step {
	return Step{
		Name: "查询目标镜像",
		Type: StepToolCall,
		Tool: "DescribeCompShareImages",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			return map[string]any{
				"CompShareImageId": wfCtx.Params["CompShareImageId"],
			}, nil
		},
		CheckResult: func(_ *Context, result map[string]any) (bool, string) {
			if extractImageName(result) == "" {
				return false, "未找到目标镜像，请确认镜像 ID 是否正确。"
			}
			return true, ""
		},
	}
}

func stepConfirmReinstall() Step {
	return Step{
		Name: "确认重装",
		Type: StepConfirm,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			summary := extractInstanceSummary(wfCtx.Result("查询实例"))
			summary["target_image_id"] = wfCtx.Params["CompShareImageId"]
			imageResult := wfCtx.Result("查询目标镜像")
			if name := extractImageName(imageResult); name != "" {
				summary["target_image_name"] = name
			}
			summary["warning"] = "⚠️ 重装系统会清除系统盘上的所有数据，数据盘不受影响。请确保重要数据已备份。"
			return summary, nil
		},
	}
}

func stepReinstallInstance() Step {
	return Step{
		Name: "重装系统",
		Type: StepToolCall,
		Tool: "ReinstallCompShareInstance",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			queried := wfCtx.Result("查询实例")
			args := map[string]any{
				"Region":           extractInstanceRegion(queried, defaultRegion),
				"Zone":             extractInstanceZone(queried, defaultZone),
				"UHostId":          wfCtx.Params["UHostId"],
				"CompShareImageId": wfCtx.Params["CompShareImageId"],
			}
			if pw, ok := wfCtx.Params["Password"].(string); ok && pw != "" {
				args["Password"] = base64.StdEncoding.EncodeToString([]byte(pw))
				args["LoginMode"] = "Password"
			}
			return args, nil
		},
	}
}

func extractImageName(result map[string]any) string {
	if result == nil {
		return ""
	}
	imageSet, ok := result["ImageSet"].([]any)
	if !ok || len(imageSet) == 0 {
		return ""
	}
	first, ok := imageSet[0].(map[string]any)
	if !ok {
		return ""
	}
	if name, ok := first["Name"].(string); ok && name != "" {
		return name
	}
	if name, ok := first["CompShareImageName"].(string); ok && name != "" {
		return name
	}
	return ""
}
