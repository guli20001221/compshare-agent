package workflow

import (
	"encoding/base64"
	"fmt"
	"math"
	"strings"
)

func ReinstallInstanceDef() *Definition {
	return &Definition{
		Name:        "ReinstallInstanceWorkflow",
		Description: "查询实例 → 查询目标镜像 → 确认重装 → 重装系统",
		Steps: []Step{
			stepQueryForReinstall(),
			stepQuerySupportZones(),
			stepQueryPlatformTargetImage(),
			stepQueryCommunityTargetImage(),
			stepQueryCustomTargetImage(),
			stepQuerySharingTargetImage(),
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
		CheckResult: func(_ *Context, result map[string]any) CheckOutcome {
			state := extractInstanceState(result)
			switch state {
			case "Stopped":
				return CheckPassed()
			case "":
				return CheckFailed("未找到该实例。")
			case "Running":
				return CheckFailed("实例当前正在运行，重装系统需要先关机。")
			case "Stopping":
				return CheckFailed("实例正在关机中，请稍后再试。")
			default:
				return CheckFailed(fmt.Sprintf("实例当前状态为「%s」，仅 Stopped 状态可以重装。", state))
			}
		},
	}
}

func stepQueryPlatformTargetImage() Step {
	return Step{
		Name:     "查询平台目标镜像",
		Type:     StepToolCall,
		Tool:     "DescribeCompShareImages",
		Optional: true,
		SkipIf: func(wfCtx *Context) (bool, error) {
			return reinstallShouldSkipSource(wfCtx, "platform"), nil
		},
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			args, ok := reinstallImageLookupArgs(wfCtx, "platform")
			if !ok {
				return nil, NewMissingSlotError("重装系统需要指定目标镜像。", "image_id")
			}
			return args, nil
		},
	}
}

func stepQueryCommunityTargetImage() Step {
	return Step{
		Name:     "查询社区目标镜像",
		Type:     StepToolCall,
		Tool:     "DescribeCommunityImages",
		Optional: true,
		SkipIf: func(wfCtx *Context) (bool, error) {
			return reinstallShouldSkipSource(wfCtx, "community"), nil
		},
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			args, ok := reinstallImageLookupArgs(wfCtx, "community")
			if !ok {
				return nil, NewMissingSlotError("重装系统需要指定目标镜像。", "image_id")
			}
			return args, nil
		},
	}
}

func stepQueryCustomTargetImage() Step {
	return Step{
		Name:     "查询自制目标镜像",
		Type:     StepToolCall,
		Tool:     "DescribeCompShareCustomImages",
		Optional: true,
		SkipIf: func(wfCtx *Context) (bool, error) {
			return reinstallShouldSkipSource(wfCtx, "custom"), nil
		},
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			args, ok := reinstallImageLookupArgs(wfCtx, "custom")
			if !ok {
				return nil, NewMissingSlotError("重装系统需要指定目标镜像。", "image_id")
			}
			return args, nil
		},
	}
}

func stepQuerySharingTargetImage() Step {
	return Step{
		Name:     "查询共享目标镜像",
		Type:     StepToolCall,
		Tool:     "DescribeCompShareSharingImages",
		Optional: true,
		SkipIf: func(wfCtx *Context) (bool, error) {
			return reinstallShouldSkipSource(wfCtx, "sharing"), nil
		},
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			args, ok := reinstallImageLookupArgs(wfCtx, "sharing")
			if !ok {
				return nil, NewMissingSlotError("重装系统需要指定目标镜像。", "image_id")
			}
			return args, nil
		},
	}
}

func stepConfirmReinstall() Step {
	return Step{
		Name: "确认重装",
		Type: StepConfirm,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			if strings.TrimSpace(paramStr(wfCtx.Params, "CompShareImageId", "")) == "" && strings.TrimSpace(paramStr(wfCtx.Params, "ImageName", "")) == "" {
				return nil, NewMissingSlotError("重装系统需要指定目标镜像。", "image_id")
			}
			image, ok := targetReinstallImage(wfCtx)
			if !ok {
				return nil, fmt.Errorf("未找到目标镜像，请确认镜像名称或 ID 是否正确。")
			}
			wfCtx.Params["CompShareImageId"] = image.ID
			queried := wfCtx.Result("查询实例")
			if (isPodInstanceResult(queried) || isContainerInstanceResult(queried)) && !image.Container {
				return nil, fmt.Errorf("Pod 实例重装必须选择容器镜像；当前镜像「%s」不是容器镜像。", image.Name)
			}
			if requiredGB := image.RequiredSystemDiskGB(); requiredGB > 0 {
				if currentGB := currentSystemDiskGB(queried); currentGB > 0 && currentGB < requiredGB {
					return nil, fmt.Errorf("目标镜像「%s」需要约 %.0fGB 系统盘，当前系统盘 %.0fGB，请先扩容系统盘或选择更小的镜像。", image.Name, requiredGB, currentGB)
				}
			}
			summary := extractInstanceSummary(queried)
			summary["target_image_id"] = wfCtx.Params["CompShareImageId"]
			summary["target_image_name"] = image.Name
			summary["target_image_source"] = image.Source
			summary["target_image_container"] = image.Container
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
				"UHostId":          wfCtx.Params["UHostId"],
				"CompShareImageId": wfCtx.Params["CompShareImageId"],
			}
			if _, err := addRequiredPodPlacementArgs(args, queried, wfCtx.Result("查询支持区")); err != nil {
				return nil, err
			}
			if pw, ok := wfCtx.Params["Password"].(string); ok && pw != "" {
				args["Password"] = base64.StdEncoding.EncodeToString([]byte(pw))
				args["LoginMode"] = "Password"
			}
			return args, nil
		},
	}
}

type reinstallImageInfo struct {
	ID        string
	Name      string
	Source    string
	Container bool
	SizeMB    float64
}

func (info reinstallImageInfo) RequiredSystemDiskGB() float64 {
	if info.SizeMB <= 0 {
		return 0
	}
	return math.Ceil(info.SizeMB / 1024)
}

func reinstallImageLookupArgs(wfCtx *Context, source string) (map[string]any, bool) {
	if id := strings.TrimSpace(paramStr(wfCtx.Params, "CompShareImageId", "")); id != "" {
		return map[string]any{"CompShareImageId": id}, true
	}
	name := strings.TrimSpace(paramStr(wfCtx.Params, "ImageName", ""))
	if name == "" {
		return nil, false
	}
	switch source {
	case "platform":
		return map[string]any{"Name": name, "Limit": 100}, true
	case "community":
		return map[string]any{"FuzzySearch": name, "Limit": 30}, true
	case "custom", "sharing":
		return map[string]any{"Limit": 100}, true
	default:
		return map[string]any{"Limit": 100}, true
	}
}

func reinstallShouldSkipSource(wfCtx *Context, source string) bool {
	selected := reinstallSelectedImageSource(wfCtx)
	return selected != "" && selected != source
}

func reinstallSelectedImageSource(wfCtx *Context) string {
	source := strings.ToLower(strings.TrimSpace(paramStr(wfCtx.Params, "ImageSource", "")))
	switch source {
	case "platform", "community", "custom":
		return source
	case "shared", "sharing":
		return "sharing"
	default:
		return ""
	}
}

func targetReinstallImage(wfCtx *Context) (reinstallImageInfo, bool) {
	want := paramStr(wfCtx.Params, "CompShareImageId", "")
	wantName := paramStr(wfCtx.Params, "ImageName", "")
	for _, item := range []struct {
		step   string
		source string
	}{
		{step: "查询平台目标镜像", source: "platform"},
		{step: "查询社区目标镜像", source: "community"},
		{step: "查询自制目标镜像", source: "custom"},
		{step: "查询共享目标镜像", source: "sharing"},
	} {
		if reinstallShouldSkipSource(wfCtx, item.source) {
			continue
		}
		if img, ok := findReinstallImage(wfCtx.Result(item.step), want, wantName, item.source); ok {
			return img, true
		}
	}
	return reinstallImageInfo{}, false
}

func findReinstallImage(result map[string]any, wantID, wantName, source string) (reinstallImageInfo, bool) {
	if result == nil {
		return reinstallImageInfo{}, false
	}
	for _, img := range imageSetMaps(result) {
		if info, ok := reinstallImageFromMap(img, source); ok && reinstallImageMatches(info, wantID, wantName) {
			return info, true
		}
	}
	for _, img := range communityImageMaps(result) {
		if info, ok := reinstallImageFromMap(img, source); ok && reinstallImageMatches(info, wantID, wantName) {
			return info, true
		}
	}
	return reinstallImageInfo{}, false
}

func reinstallImageMatches(info reinstallImageInfo, wantID, wantName string) bool {
	if strings.TrimSpace(wantID) != "" {
		return imageIDMatches(info.ID, wantID)
	}
	want := strings.ToLower(strings.TrimSpace(wantName))
	if want == "" {
		return false
	}
	name := strings.ToLower(strings.TrimSpace(info.Name))
	return name == want || strings.Contains(name, want)
}

func imageSetMaps(result map[string]any) []map[string]any {
	raw, ok := result["ImageSet"].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func communityImageMaps(result map[string]any) []map[string]any {
	groups, ok := result["CompshareImageGroup"].([]any)
	if !ok {
		return nil
	}
	out := []map[string]any{}
	for _, rawGroup := range groups {
		group, ok := rawGroup.(map[string]any)
		if !ok {
			continue
		}
		groupName, _ := group["ImageName"].(string)
		data, ok := group["Data"].([]any)
		if !ok {
			continue
		}
		for _, rawData := range data {
			img, ok := rawData.(map[string]any)
			if !ok {
				continue
			}
			if _, ok := img["ImageName"]; !ok && groupName != "" {
				img["ImageName"] = groupName
			}
			out = append(out, img)
		}
	}
	return out
}

func reinstallImageFromMap(img map[string]any, source string) (reinstallImageInfo, bool) {
	id := firstStringValue(img, "CompShareImageId", "ImageId", "Id")
	if id == "" {
		return reinstallImageInfo{}, false
	}
	name := firstStringValue(img, "Name", "CompShareImageName", "ImageName")
	if name == "" {
		name = id
	}
	return reinstallImageInfo{
		ID:        id,
		Name:      name,
		Source:    source,
		Container: paramBool(img, "Container", false) || paramBool(img, "IsContainer", false),
		SizeMB:    diskNumber(img, "Size", "ActualSize", "ImageSize"),
	}, true
}

func currentSystemDiskGB(result map[string]any) float64 {
	for _, disk := range extractDiskSet(result) {
		if isBootDisk(disk) {
			return diskNumber(disk, "Size", "DiskSize", "Capacity")
		}
	}
	return 0
}

func firstStringValue(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func imageIDMatches(got, want string) bool {
	return want == "" || strings.EqualFold(got, want)
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
