package workflow

import (
	"encoding/base64"
	"fmt"
	"math"
	"strings"

	"github.com/compshare-agent/internal/deployment"
)

func ReinstallInstanceDef() *Definition {
	return &Definition{
		Name: "ReinstallInstanceWorkflow",
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
			password, _ := wfCtx.Params["Password"].(string)
			passwordConfigured := strings.TrimSpace(password) != ""
			wfCtx.Params["PasswordConfigured"] = passwordConfigured
			summary["password_will_change"] = passwordConfigured
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
			} else if configured, _ := wfCtx.Params["PasswordConfigured"].(bool); configured {
				return nil, fmt.Errorf("已确认设置新密码，但安全密码输入已丢失，拒绝执行重装。")
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

// targetReinstallImage resolves the reinstall target through the ONE image
// interpreter (deployment.ResolveImage), against each candidate source's query
// result in turn. It replaces the reinstall-specific matcher (exact-id OR
// name==want||Contains) — the third image interpreter the convergence removes. The
// snapshot carries the image size too, so the disk-fit check reads it from the same
// resolved row rather than a parallel walk of the response.
func targetReinstallImage(wfCtx *Context) (reinstallImageInfo, bool) {
	id := paramStr(wfCtx.Params, "CompShareImageId", "")
	name := paramStr(wfCtx.Params, "ImageName", "")
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
		result := wfCtx.Result(item.step)
		if result == nil {
			continue
		}
		snap := reinstallSnapshot(result, item.source)
		res := deployment.ResolveImage(snap, deployment.ImageRequest{
			ID:     id,
			Name:   name,
			Source: item.source,
			// The platform and community reinstall queries filter by name (Name= /
			// FuzzySearch=), so a non-exact request recommends the best returned row;
			// the custom/sharing queries return the full list, so client-side name
			// relevance still applies there.
			Prefiltered: item.source == "platform" || item.source == "community",
		})
		sel, ok := reinstallSelection(res)
		if !ok {
			continue
		}
		entry, _ := snap.ByID(sel.ID)
		return reinstallImageInfo{
			ID:        sel.ID,
			Name:      sel.Name,
			Source:    item.source,
			Container: sel.Container,
			SizeMB:    entry.SizeMB,
		}, true
	}
	return reinstallImageInfo{}, false
}

// reinstallSelection picks the resolved image, or the top recommended candidate for
// a not_found/ambiguous named request, or nothing when the source has no match.
func reinstallSelection(res deployment.ImageResolution) (deployment.ImageSelection, bool) {
	if res.Status == deployment.ResolutionResolved {
		return res.Selection, true
	}
	if len(res.Candidates) > 0 {
		return res.Candidates[0], true
	}
	return deployment.ImageSelection{}, false
}

// reinstallSnapshot builds the turn's image catalog for one reinstall source from
// its query result, parsing the grouped community shape or the flat ImageSet shape.
func reinstallSnapshot(result map[string]any, source string) *deployment.ImageCatalogSnapshot {
	if source == "community" {
		return deployment.NewImageCatalogSnapshot(true, deployment.ParseCommunityImageEntries(result))
	}
	return deployment.NewImageCatalogSnapshot(true, deployment.ParsePlatformImageEntries(result, source))
}

func currentSystemDiskGB(result map[string]any) float64 {
	for _, disk := range extractDiskSet(result) {
		if isBootDisk(disk) {
			return diskNumber(disk, "Size", "DiskSize", "Capacity")
		}
	}
	return 0
}
