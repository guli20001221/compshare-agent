package workflow

import (
	"fmt"
	"math"
	"strings"

	"github.com/compshare-agent/internal/deployment"
)

func ReinstallInstanceDef() *Definition {
	return &Definition{
		Name:             "ReinstallInstanceWorkflow",
		NeedsZoneCatalog: true,
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
		FailureDraft: reinstallFailureDraft,
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
			case "Running", "Stopping", "Stopped":
				// The effective state contract depends on both the instance kind and
				// the resolved target image. Pod reinstall can converge Running or
				// Stopping to Stopped itself, while UHost container->container requires
				// Running. Validate that cross-product after image resolution.
				return CheckPassed()
			case "":
				return CheckFailed("未找到该实例。")
			default:
				return CheckFailed(fmt.Sprintf("实例当前状态为「%s」，暂不支持重装。", state))
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
			if isPodInstanceResult(queried) && !image.Container {
				return nil, fmt.Errorf("Pod 实例重装必须选择容器镜像；当前镜像「%s」不是容器镜像。", image.Name)
			}
			if strings.TrimSpace(image.ImageType) != "" {
				zoneEntry, err := workflowZoneEntry(wfCtx, firstInstanceField(queried, "Zone"))
				if err != nil {
					return nil, err
				}
				if !zoneEntry.SupportsImageType(image.ImageType) {
					return nil, fmt.Errorf("目标可用区 %s 当前不支持 %s 类型镜像，无法重装。", zoneEntry.DisplayName, image.ImageType)
				}
			}
			if err := validateReinstallStateForPath(queried, image); err != nil {
				return nil, err
			}
			// In no-card UHost mode DescribeCompShareInstance exposes only the display
			// GPU name and MachineType=O. The upstream reinstall gate validates the
			// hidden UhostType instead; it is not present in this API response, so the
			// Agent cannot prove compatibility from the catalog. Refuse before a
			// destructive confirmation rather than knowingly send a request that the
			// upstream rejects with 226603.
			if strings.EqualFold(firstInstanceField(queried, "MachineType"), "O") {
				return nil, fmt.Errorf("实例当前处于无卡运行模式，重装接口无法核实底层 GPU 机型与目标镜像的兼容性。请先恢复带卡运行并关机，再发起重装。")
			}
			// SupportedGpuTypes is advisory for create capacity ranking, but the
			// upstream ReinstallCompShareInstance implementation explicitly rejects a
			// non-member with RetCode 226603. Enforce that operation-specific contract
			// before asking the user to approve a destructive reinstall.
			if gpuType := reinstallCurrentGPUType(queried); gpuType != "" &&
				len(image.SupportedGPUTypes) > 0 && !containsFold(image.SupportedGPUTypes, gpuType) {
				return nil, fmt.Errorf("目标镜像「%s」不支持当前实例的 GPU 机型 %s，请选择支持该机型的镜像后重试。", image.Name, gpuType)
			}
			if requiredGB := image.RequiredSystemDiskGB(); requiredGB > 0 {
				if currentGB := currentSystemDiskGB(queried); currentGB > 0 && currentGB < requiredGB {
					return nil, fmt.Errorf("目标镜像「%s」需要约 %.0fGB 系统盘，当前系统盘 %.0fGB，请先扩容系统盘或选择更小的镜像。", image.Name, requiredGB, currentGB)
				}
			}
			if strings.EqualFold(image.Source, "community") && !image.PriceKnown {
				return nil, fmt.Errorf("目标社区镜像未返回价格，无法生成完整的付费重装确认卡。请稍后重试。")
			}
			summary := extractInstanceSummary(queried)
			summary["target_image_id"] = wfCtx.Params["CompShareImageId"]
			summary["target_image_name"] = image.Name
			summary["target_image_source"] = image.Source
			summary["target_image_container"] = image.Container
			if strings.EqualFold(image.Source, "community") {
				if image.Price > 0 {
					summary["target_image_price"] = fmt.Sprintf("¥%.2f（镜像目录标价，实际费用以平台结算为准）", image.Price)
				} else {
					summary["target_image_price"] = "免费"
				}
			}
			// The public request type still contains Password/LoginMode for wire
			// compatibility, but neither the Pod nor UHost reinstall implementation
			// consumes those fields. UHost reuses its stored credential (or generates
			// one internally when converting a host image to a container image); Pod
			// likewise owns credential persistence. Do not offer a password change the
			// operation cannot honour.
			summary["credential_handling"] = "由平台沿用或按目标镜像类型在内部生成；本次重装不接受新密码"
			summary["warning"] = reinstallImpactWarning(queried, image)
			return summary, nil
		},
	}
}

func validateReinstallStateForPath(queried map[string]any, image reinstallImageInfo) error {
	state := extractInstanceState(queried)
	if isPodInstanceResult(queried) {
		switch state {
		case "Running", "Stopping", "Stopped":
			return nil
		default:
			return fmt.Errorf("Pod 实例当前状态为「%s」，仅 Running、Stopping 或 Stopped 状态可以重装。", state)
		}
	}

	// UHost container->container replaces only the managed container and the
	// upstream implementation explicitly requires a Running domain. Every path
	// that changes between host/container images, or replaces a host image,
	// reinstalls the UHost system disk and therefore requires Stopped.
	if isContainerInstanceResult(queried) && image.Container {
		if state != "Running" {
			return fmt.Errorf("UHost 容器实例原地重装容器镜像需要实例处于 Running 状态；当前状态为「%s」。", state)
		}
		return nil
	}
	if state != "Stopped" {
		return fmt.Errorf("该重装路径会替换 UHost 系统盘，需要实例先关机；当前状态为「%s」。", state)
	}
	return nil
}

func reinstallImpactWarning(queried map[string]any, image reinstallImageInfo) string {
	if isPodInstanceResult(queried) {
		state := extractInstanceState(queried)
		prefix := "Pod 将重建并重新启动"
		if state == "Running" {
			prefix = "Pod 会先自动停止，再重建并重新启动"
		} else if state == "Stopping" {
			prefix = "Pod 会等待关机完成，再重建并重新启动"
		}
		return "⚠️ " + prefix + "；平台会复用现有系统盘和 CFS 绑定、替换容器镜像，并重建为镜像默认端口配置，原有自定义端口不会自动保留。"
	}
	if isContainerInstanceResult(queried) && image.Container {
		return "⚠️ 将替换当前 UHost 内的托管容器及其容器文件系统，服务会中断；不会重装 UHost 系统盘，数据盘不受影响。请先备份容器内重要数据。"
	}
	return "⚠️ 重装会清除 UHost 系统盘上的所有数据，数据盘不受影响。请确保重要数据已备份。"
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
			return args, nil
		},
	}
}

type reinstallImageInfo struct {
	ID                string
	Name              string
	Source            string
	ImageType         string
	Container         bool
	SupportedGPUTypes []string
	SizeMB            float64
	Price             float64
	PriceKnown        bool
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
			ID:                sel.ID,
			Name:              sel.Name,
			Source:            item.source,
			ImageType:         entry.ImageType,
			Container:         sel.Container,
			SupportedGPUTypes: append([]string(nil), entry.SupportedGPUTypes...),
			SizeMB:            entry.SizeMB,
			Price:             entry.Price,
			PriceKnown:        entry.PriceKnown,
		}, true
	}
	return reinstallImageInfo{}, false
}

func reinstallFailureDraft(wfCtx *Context) map[string]any {
	if wfCtx == nil {
		return nil
	}
	queried := wfCtx.Result("查询实例")
	image, _ := targetReinstallImage(wfCtx)
	return map[string]any{
		"UHostId":         strings.TrimSpace(paramStr(wfCtx.Params, "UHostId", "")),
		"InitialState":    extractInstanceState(queried),
		"IsPod":           isPodInstanceResult(queried),
		"TargetImageId":   image.ID,
		"TargetImageName": image.Name,
	}
}

// reinstallCurrentGPUType returns the GPU identity the upstream reinstall API
// validates. A no-card instance may carry the original sellable configuration
// under SrcInstanceConfig, so the current empty GpuType must not erase that fact.
func reinstallCurrentGPUType(result map[string]any) string {
	host, ok := firstInstance(result)
	if !ok {
		return ""
	}
	if gpu := strings.TrimSpace(stringFieldAny(host["GpuType"])); gpu != "" {
		return gpu
	}
	if gpu := strings.TrimSpace(stringFieldAny(host["GPUType"])); gpu != "" {
		return gpu
	}
	if source, ok := sourceInstanceConfig(host); ok {
		if gpu := strings.TrimSpace(stringFieldAny(source["GpuType"])); gpu != "" {
			return gpu
		}
		return strings.TrimSpace(stringFieldAny(source["GPUType"]))
	}
	return ""
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
