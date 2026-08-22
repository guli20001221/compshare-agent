package workflow

import (
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/deployment"
)

// CloneCustomImageDef synchronises one catalog-verified custom image into one
// confirmed target zone. The upstream supports several target zones per request,
// but one workflow run intentionally owns one target: the confirmation card and
// sealed contract then describe exactly one write destination.
func CloneCustomImageDef() *Definition {
	return &Definition{
		Name:               "CloneCustomImageWorkflow",
		ImageCatalogSource: "custom",
		Steps: []Step{
			stepConfirmCloneCustomImage(),
			stepCloneCustomImage(),
			stepGetCustomImageSyncProgress(),
		},
		ResultData: cloneCustomImageResultData,
	}
}

func stepConfirmCloneCustomImage() Step {
	return Step{
		Name: "确认克隆自制镜像",
		Type: StepConfirm,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			source, target, err := cloneCustomImageSelection(wfCtx)
			if err != nil {
				return nil, err
			}
			name := paramStr(wfCtx.Params, "TargetImageName", "")
			if name == "" {
				return nil, NewMissingSlotError("克隆自制镜像需要指定目标镜像名称。", "target_image_name")
			}
			name, err = validatedCompShareResourceName(name, "目标镜像名称", 50)
			if err != nil {
				return nil, err
			}
			summary := map[string]any{
				"workflow":               "CloneCustomImageWorkflow",
				"SourceCompShareImageId": source.ID,
				"SourceImageName":        source.Name,
				"SourceZone":             source.Zone,
				"TargetZone":             target.Placement.Zone,
				"TargetZoneName":         target.DisplayName,
				"TargetImageName":        name,
			}
			if description := strings.TrimSpace(paramStr(wfCtx.Params, "TargetImageDescription", "")); description != "" {
				summary["TargetImageDescription"] = description
			}
			return summary, nil
		},
	}
}

func stepCloneCustomImage() Step {
	return Step{
		Name: "克隆自制镜像",
		Type: StepToolCall,
		Tool: "SyncCompShareCustomImage",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			source, target, err := cloneCustomImageSelection(wfCtx)
			if err != nil {
				return nil, err
			}
			name, err := validatedCompShareResourceName(paramStr(wfCtx.Params, "TargetImageName", ""), "目标镜像名称", 50)
			if err != nil {
				return nil, err
			}
			args := map[string]any{
				"SourceCompShareImageId": source.ID,
				"TargetImageName":        name,
				"TargetZoneIds":          []uint32{target.Placement.ZoneID},
			}
			if description := strings.TrimSpace(paramStr(wfCtx.Params, "TargetImageDescription", "")); description != "" {
				args["TargetImageDescription"] = description
			}
			return args, nil
		},
		CheckResult: func(_ *Context, result map[string]any) CheckOutcome {
			id, ok, message := clonedCustomImageResult(result)
			if !ok {
				if message == "" {
					message = "上游未创建自制镜像同步任务。"
				}
				return CheckFailed(message)
			}
			if id == "" {
				return CheckFailed("克隆成功结果未返回目标 CompShareImageId。")
			}
			return CheckPassed()
		},
	}
}

func stepGetCustomImageSyncProgress() Step {
	return Step{
		Name:     "查询镜像同步进度",
		Type:     StepToolCall,
		Tool:     "DescribeCompShareCustomImageSyncDetail",
		Optional: true,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			id, ok, _ := clonedCustomImageResult(wfCtx.Result("克隆自制镜像"))
			if !ok || id == "" {
				return nil, fmt.Errorf("目标 CompShareImageId 为空，无法查询同步进度")
			}
			_, target, err := cloneCustomImageSelection(wfCtx)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"CompShareImageId": id,
				"zone_id":          target.Placement.ZoneID,
			}, nil
		},
	}
}

func cloneCustomImageSelection(wfCtx *Context) (deployment.ImageCatalogEntry, deployment.ZoneCatalogEntry, error) {
	imageCatalog := wfCtx.ImageCatalog()
	if !imageCatalog.Available() {
		return deployment.ImageCatalogEntry{}, deployment.ZoneCatalogEntry{}, fmt.Errorf("自制镜像目录当前不可用，无法安全克隆")
	}
	sourceID := strings.TrimSpace(paramStr(wfCtx.Params, "CompShareImageId", ""))
	source, ok := imageCatalog.ByID(sourceID)
	if !ok || !strings.EqualFold(source.Source, "custom") {
		return deployment.ImageCatalogEntry{}, deployment.ZoneCatalogEntry{}, fmt.Errorf("源自制镜像 %s 不在当前账号的镜像目录中", sourceID)
	}
	if source.Status != "" && !strings.EqualFold(source.Status, deployment.ImageStatusAvailable) {
		return deployment.ImageCatalogEntry{}, deployment.ZoneCatalogEntry{}, fmt.Errorf("源自制镜像当前状态为 %s，只有 Available 状态可以克隆", source.Status)
	}
	target, err := workflowZoneEntry(wfCtx, paramStr(wfCtx.Params, "Zone", ""))
	if err != nil {
		return deployment.ImageCatalogEntry{}, deployment.ZoneCatalogEntry{}, err
	}
	if target.Placement.ZoneID == 0 {
		return deployment.ImageCatalogEntry{}, deployment.ZoneCatalogEntry{}, fmt.Errorf("目标可用区缺少内部编号，无法安全克隆")
	}
	// A VM custom image is backed by a UHost image and cannot be migrated into a
	// Pod-only zone. Container custom images use the independent registry-sync
	// path and remain valid in both kinds of zone. This is derived entirely from
	// the catalog image form and the typed zone placement; no zone-id table is
	// duplicated here.
	if !source.Container && target.Placement.IsPod {
		return deployment.ImageCatalogEntry{}, deployment.ZoneCatalogEntry{}, fmt.Errorf("虚机自制镜像不能克隆到容器可用区 %s，请选择虚机可用区", target.DisplayName)
	}
	if source.Container {
		if target.DisableImageSync {
			return deployment.ImageCatalogEntry{}, deployment.ZoneCatalogEntry{}, fmt.Errorf("目标可用区 %s 当前禁止容器自制镜像同步", target.DisplayName)
		}
		sourceZone, ok := workflowZoneEntryByNumericID(wfCtx, source.ZoneID)
		if ok && sourceZone.DisableImageSync {
			return deployment.ImageCatalogEntry{}, deployment.ZoneCatalogEntry{}, fmt.Errorf("源可用区 %s 当前禁止容器自制镜像同步", sourceZone.DisplayName)
		}
	}
	if (source.ZoneID != 0 && source.ZoneID == target.Placement.ZoneID) ||
		(source.Zone != "" && strings.EqualFold(source.Zone, target.Placement.Zone)) {
		return deployment.ImageCatalogEntry{}, deployment.ZoneCatalogEntry{}, fmt.Errorf("目标可用区不能与源镜像所在可用区相同")
	}
	return source, target, nil
}

func workflowZoneEntryByNumericID(wfCtx *Context, zoneID uint32) (deployment.ZoneCatalogEntry, bool) {
	if wfCtx == nil || zoneID == 0 {
		return deployment.ZoneCatalogEntry{}, false
	}
	catalog := wfCtx.ZoneCatalog()
	if !catalog.Available() {
		return deployment.ZoneCatalogEntry{}, false
	}
	for _, zone := range catalog.Zones() {
		entry, ok := catalog.Entry(zone)
		if ok && entry.Placement.ZoneID == zoneID {
			return entry, true
		}
	}
	return deployment.ZoneCatalogEntry{}, false
}

func clonedCustomImageResult(result map[string]any) (id string, ok bool, message string) {
	if intValue(result["SuccessCount"]) < 1 {
		return "", false, firstSyncResultMessage(result)
	}
	rows, _ := result["Results"].([]any)
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok || intValue(row["RetCode"]) != 0 {
			continue
		}
		id, _ := row["CompShareImageId"].(string)
		return strings.TrimSpace(id), true, ""
	}
	return "", false, firstSyncResultMessage(result)
}

func firstSyncResultMessage(result map[string]any) string {
	rows, _ := result["Results"].([]any)
	for _, raw := range rows {
		if row, ok := raw.(map[string]any); ok {
			if message, _ := row["Message"].(string); strings.TrimSpace(message) != "" {
				return strings.TrimSpace(message)
			}
		}
	}
	return ""
}

func intValue(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}

func cloneCustomImageResultData(wfCtx *Context) map[string]any {
	id, ok, _ := clonedCustomImageResult(wfCtx.Result("克隆自制镜像"))
	if !ok || id == "" {
		return nil
	}
	out := map[string]any{"CompShareImageId": id}
	if progress := wfCtx.Result("查询镜像同步进度"); len(progress) > 0 {
		out["Progress"] = progress
	}
	return out
}
