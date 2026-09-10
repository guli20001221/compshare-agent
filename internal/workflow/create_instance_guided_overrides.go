package workflow

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/compshare-agent/internal/deployment"
)

// Applying what the user changed on a card back into the workflow params.
// Keys are restricted to that form's field set, so an override cannot reach
// a parameter its card never showed. Changing an upstream answer clears the
// downstream selections it invalidated rather than leaving a stale image or
// spec attached to a specification the user has just replaced.

// applyCreateOverrides merges validated form overrides into the workflow
// params. Keys are restricted to the form's field set; anything else is
// rejected (defense in depth on top of ConfirmForm.ValidateOverrides).
func applyCreateOverrides(wfCtx *Context, overrides map[string]string) error {
	_, zoneOverridden := overrides["Zone"]
	for k, v := range overrides {
		switch k {
		case "GpuType":
			wfCtx.Params["GpuType"] = v
			// A pinned CPU/Memory combo (and an auto-resolved zone) belongs to
			// the PREVIOUS GPU; drop them so resolveTargetSpec re-defaults for
			// the new card. Nothing is silent: the refreshed confirm card shows
			// the re-resolved CPU/Memory/Zone before anything is created.
			delete(wfCtx.Params, "Cpu")
			delete(wfCtx.Params, "Memory")
			if !zoneOverridden {
				delete(wfCtx.Params, "Zone")
			}
		case "Zone":
			wfCtx.Params["Zone"] = v
		case "ChargeType":
			wfCtx.Params["ChargeType"] = v
		case "ImageId":
			// Thread the exact id — pickImageId prefers it everywhere
			// (capacity / price / create), so the validated image is the one
			// that gets built.
			wfCtx.Params["CompShareImageId"] = v
			if name := imageNameByID(createImageResult(wfCtx), v); name != "" {
				wfCtx.Params["ImageName"] = name
			}
		default:
			return fmt.Errorf("不支持修改字段 %s", k)
		}
	}
	return nil
}

func applyGuidedChargeTypeOverrides(wfCtx *Context, overrides map[string]string) error {
	value, ok := overrides["ChargeType"]
	if !ok {
		return nil
	}
	if !enabledOptionExists(guidedChargeTypeOptions(wfCtx), value) {
		return fmt.Errorf("暂不支持该计费方式")
	}
	wfCtx.Params["ChargeType"] = deployment.NormalizeChargeType(value)
	markGuidedStepReached(wfCtx, guidedStepChargeType)
	return nil
}

func applyGuidedGPUOverrides(wfCtx *Context, overrides map[string]string) error {
	for k, v := range overrides {
		switch k {
		case "GpuType":
			old := paramStr(wfCtx.Params, "GpuType", "")
			wfCtx.Params["GpuType"] = v
			if !strings.EqualFold(old, v) {
				delete(wfCtx.Params, "Gpu")
				delete(wfCtx.Params, "Cpu")
				delete(wfCtx.Params, "Memory")
				delete(wfCtx.Params, "Zone")
				if supported := currentImageSupportedGPUs(wfCtx.Params, createImageResult(wfCtx)); len(supported) > 0 && !containsFold(supported, v) {
					clearGuidedImageSelection(wfCtx)
				}
			}
		default:
			return fmt.Errorf("不支持修改字段 %s", k)
		}
	}
	markGuidedStepReached(wfCtx, guidedStepGPU)
	return nil
}

func applyGuidedZoneOverrides(wfCtx *Context, overrides map[string]string) error {
	for k, v := range overrides {
		switch k {
		case "Zone":
			old := paramStr(wfCtx.Params, "Zone", "")
			wfCtx.Params["Zone"] = v
			syncGuidedZoneMeta(wfCtx, v)
			if !strings.EqualFold(old, v) {
				delete(wfCtx.Params, "GuidedZoneLocked")
				delete(wfCtx.Params, "Gpu")
				delete(wfCtx.Params, "Cpu")
				delete(wfCtx.Params, "Memory")
			}
		default:
			return fmt.Errorf("不支持修改字段 %s", k)
		}
	}
	markGuidedStepReached(wfCtx, guidedStepZone)
	return nil
}

func applyGuidedGPUCountOverrides(wfCtx *Context, overrides map[string]string) error {
	for k, v := range overrides {
		switch k {
		case "Gpu":
			gpu, err := strconv.ParseFloat(v, 64)
			if err != nil || gpu <= 0 {
				return fmt.Errorf("卡数量选择无效")
			}
			old := paramNum(wfCtx.Params, "Gpu", 0)
			wfCtx.Params["Gpu"] = gpu
			if old != gpu {
				delete(wfCtx.Params, "Cpu")
				delete(wfCtx.Params, "Memory")
			}
		default:
			return fmt.Errorf("不支持修改字段 %s", k)
		}
	}
	markGuidedStepReached(wfCtx, guidedStepGPUCount)
	return nil
}

func applyGuidedCpuMemoryOverrides(wfCtx *Context, overrides map[string]string) error {
	for k, v := range overrides {
		switch k {
		case "CpuMemory":
			zone, gpu, cpu, memoryMB, err := parseGuidedSpecKey(v)
			if err != nil {
				return err
			}
			wfCtx.Params["Zone"] = zone
			syncGuidedZoneMeta(wfCtx, zone)
			wfCtx.Params["Gpu"] = gpu
			wfCtx.Params["Cpu"] = cpu
			wfCtx.Params["Memory"] = memoryMB
		default:
			return fmt.Errorf("不支持修改字段 %s", k)
		}
	}
	markGuidedStepReached(wfCtx, guidedStepCPUMemory)
	return nil
}

func applyGuidedImageFacetsOverrides(wfCtx *Context, overrides map[string]string) error {
	for k, v := range overrides {
		switch k {
		case "ImageType":
			wfCtx.Params["ImageType"] = strings.TrimSpace(v)
		case "ImageCategory":
			wfCtx.Params["ImageCategory"] = strings.TrimSpace(v)
		default:
			return fmt.Errorf("不支持修改字段 %s", k)
		}
	}
	// A type/category change invalidates a previously-picked concrete image: the
	// refreshed image step re-picks from the newly-scoped candidates rather than
	// carrying a stale id/name that may not match. The source is owned by the earlier
	// source step and is never touched here. Nothing is silent: the user re-confirms
	// the refreshed card before anything is created.
	clearGuidedImageSelection(wfCtx)
	// The tag was chosen against the PREVIOUS type's candidates, so it may now select
	// nothing. Cleared = absent = "no filter" (honest absence, never "match nothing"),
	// and the tag card that follows re-asks over the new candidates.
	delete(wfCtx.Params, "ImageTag")
	markGuidedStepReached(wfCtx, guidedStepImageFacets)
	return nil
}

func applyGuidedImageTagOverrides(wfCtx *Context, overrides map[string]string) error {
	for k, v := range overrides {
		if k != "ImageTag" {
			return fmt.Errorf("不支持修改字段 %s", k)
		}
		wfCtx.Params["ImageTag"] = strings.TrimSpace(v)
	}
	clearGuidedImageSelection(wfCtx)
	markGuidedStepReached(wfCtx, guidedStepImageTag)
	return nil
}

// clearGuidedConcreteImageSelection drops only the resolved version. A selected
// family can remain while its version card is shown; callers that invalidate the
// whole catalog-derived choice use clearGuidedImageSelection instead.
func clearGuidedConcreteImageSelection(wfCtx *Context) {
	if wfCtx == nil || wfCtx.Params == nil {
		return
	}
	// A name without an ID is still the requested search, not metadata from a
	// previously selected image. Keep that query when changing catalog facets.
	if strings.TrimSpace(paramStr(wfCtx.Params, "CompShareImageId", "")) != "" {
		delete(wfCtx.Params, "ImageName")
	}
	delete(wfCtx.Params, "CompShareImageId")
	delete(wfCtx.Params, "GuidedImageLocked")
}

func clearGuidedImageSelection(wfCtx *Context) {
	if wfCtx == nil || wfCtx.Params == nil {
		return
	}
	delete(wfCtx.Params, "ImageFamily")
	clearGuidedConcreteImageSelection(wfCtx)
}

// applyGuidedImageFamilyOverrides records a series choice. A singleton family has
// already resolved the only safe concrete image, so it is locked immediately;
// multi-version families intentionally proceed to the version picker.
func applyGuidedImageFamilyOverrides(wfCtx *Context, overrides map[string]string) error {
	var selected string
	for k, v := range overrides {
		if k != "ImageFamily" {
			return fmt.Errorf("不支持修改字段 %s", k)
		}
		selected = strings.TrimSpace(v)
	}
	if selected == "" {
		return fmt.Errorf("镜像系列选择不能为空")
	}
	var family deployment.ImageFamily
	found := false
	for _, candidate := range createImageFamilies(wfCtx) {
		if candidate.Key == selected {
			family, found = candidate, true
			break
		}
	}
	if !found || len(family.Variants) == 0 {
		return fmt.Errorf("镜像系列不在当前可选范围内")
	}

	clearGuidedConcreteImageSelection(wfCtx)
	wfCtx.Params["ImageFamily"] = family.Key
	if len(family.Variants) == 1 {
		if err := applyCreateOverrides(wfCtx, map[string]string{"ImageId": family.Variants[0].ID}); err != nil {
			return err
		}
		wfCtx.Params["GuidedImageLocked"] = true
	}
	markGuidedStepReached(wfCtx, guidedStepImageFamily)
	return nil
}

// applyGuidedImageSourceOverrides applies the source-only step's edit. On an ACTUAL
// source change it clears everything derived from the PREVIOUS source's catalog — the
// ImageType/ImageTag facets and any pinned concrete image — so the source re-query +
// facets step rebuild from the newly-chosen source (cleared = absent = "no filter",
// honest absence, never "match nothing"). A same-source re-confirm preserves whatever
// was already chosen.
func applyGuidedImageSourceOverrides(wfCtx *Context, overrides map[string]string) error {
	prevSource := normalizedImageSource(paramStr(wfCtx.Params, "ImageSource", "platform"))
	sourceChanged := false
	for k, v := range overrides {
		if k != "ImageSource" {
			return fmt.Errorf("不支持修改字段 %s", k)
		}
		source := normalizedImageSource(v)
		if source != prevSource {
			sourceChanged = true
		}
		wfCtx.Params["ImageSource"] = source
	}
	if sourceChanged {
		delete(wfCtx.Params, "ImageType")
		delete(wfCtx.Params, "ImageTag")
		// The 用途 category is derived from the previous source's catalog too: the
		// platform catalog barely intersects the classification (only ComfyUI of its
		// tags is a taxonomy member) while community rows are fully classified, so a
		// category carried across a source switch can silently match nothing.
		delete(wfCtx.Params, "ImageCategory")
		clearGuidedImageSelection(wfCtx)
	}
	markGuidedStepReached(wfCtx, guidedStepImageSource)
	return nil
}

// normalizedImageSource collapses aliases to one create-supported catalog source.
func normalizedImageSource(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case imageSourceCommunity:
		return imageSourceCommunity
	case imageSourceCustom:
		return imageSourceCustom
	case "shared", imageSourceSharing:
		return imageSourceSharing
	default:
		return imageSourcePlatform
	}
}

// hasExplicitImageSelection reports a concrete image already pinned by id or name —
// distinct from hasExplicitImageIntent, which ALSO treats a non-platform source as
// "intent". The two-stage source/facets steps must show for community or custom
// browsing (source chosen, no concrete image yet), so they gate on this narrower
// predicate.
func hasExplicitImageSelection(params map[string]any) bool {
	return strings.TrimSpace(paramStr(params, "CompShareImageId", "")) != "" ||
		strings.TrimSpace(paramStr(params, "ImageName", "")) != ""
}

func applyGuidedImageOverrides(wfCtx *Context, overrides map[string]string) error {
	for k := range overrides {
		if k != "ImageId" {
			return fmt.Errorf("不支持修改字段 %s", k)
		}
	}
	if err := applyCreateOverrides(wfCtx, overrides); err != nil {
		return err
	}
	// A real picker submission locks its exact image until an edit invalidates it.
	wfCtx.Params["GuidedImageLocked"] = true
	markGuidedStepReached(wfCtx, guidedStepImage)
	return nil
}

func ensureGuidedGPUType(wfCtx *Context) (string, error) {
	current, _ := wfCtx.Params["GpuType"].(string)
	supported := currentImageSupportedGPUs(wfCtx.Params, createImageResult(wfCtx))
	locked := paramBool(wfCtx.Params, "GuidedGpuLocked", false) && current != ""
	selected, opts := guidedGPUFormOptions(wfCtx, wfCtx.Result("查询可用配比"), supported, current, locked, wfCtx.Params, wfCtx.Result("查询GPU库存"))
	// A catalog-unavailable current model is kept visible in the option list so
	// the form can explain why it changed. When no enabled replacement exists,
	// stop in BuildArgs; a BuildForm error alone would degrade to a plain card.
	// Other disabled reasons (for example image incompatibility) still reach the
	// existing explanatory card instead of being collapsed into a stock failure.
	if locked && firstEnabledValue(opts) == "" {
		for _, opt := range opts {
			if !strings.EqualFold(opt.Value, current) || opt.Meta["Sellable"] != "false" {
				continue
			}
			reason := strings.TrimSpace(opt.Reason)
			if reason == "" {
				reason = "当前不可用"
			}
			return "", fmt.Errorf("%s %s，请换一个 GPU 型号或稍后再试", current, reason)
		}
	}
	if selected == "" {
		for _, opt := range opts {
			if !opt.Disabled {
				selected = opt.Value
				break
			}
		}
	}
	if selected == "" {
		if current != "" {
			for _, opt := range opts {
				if strings.EqualFold(opt.Value, current) && opt.Disabled {
					reason := opt.Reason
					if reason == "" {
						reason = opt.Note
					}
					if reason == "" {
						reason = "当前不可用"
					}
					return "", fmt.Errorf("%s %s，请换一个 GPU 型号或稍后再试", current, reason)
				}
			}
		}
		return "", fmt.Errorf("暂无可选 GPU 型号")
	}
	if current == "" {
		wfCtx.Params["GpuType"] = selected
	}
	return selected, nil
}

func ensureGuidedZone(wfCtx *Context) (string, error) {
	gpuType, err := ensureGuidedGPUType(wfCtx)
	if err != nil {
		return "", err
	}
	current := paramStr(wfCtx.Params, "Zone", "")
	selected, opts, _ := guidedZoneFormOptions(wfCtx, wfCtx.Result("查询可用配比"), gpuType, current, wfCtx.Params, wfCtx.Result("查询GPU库存"))
	if selected == "" || len(opts) == 0 {
		return "", fmt.Errorf("%s 暂无可选可用区，请换一个 GPU 型号或稍后再试", gpuType)
	}
	if paramBool(wfCtx.Params, "GuidedZoneLocked", false) && current != "" {
		for _, opt := range opts {
			if strings.EqualFold(opt.Value, current) {
				if opt.Disabled {
					reason := opt.Reason
					if reason == "" {
						reason = opt.Note
					}
					if reason == "" {
						reason = "暂不可用"
					}
					return "", fmt.Errorf("你指定的可用区 %s 当前%s，请换一个可用区或稍后再试", zoneDisplayLabel(wfCtx, current), reason)
				}
				wfCtx.Params["Zone"] = current
				syncGuidedZoneMeta(wfCtx, current)
				return current, nil
			}
		}
		return "", fmt.Errorf("你指定的可用区 %s 当前不支持 %s，请换一个可用区或 GPU 型号", zoneDisplayLabel(wfCtx, current), gpuType)
	}
	wfCtx.Params["Zone"] = selected
	syncGuidedZoneMeta(wfCtx, selected)
	return selected, nil
}

func ensureGuidedGPUCount(wfCtx *Context) (float64, error) {
	gpuType, err := ensureGuidedGPUType(wfCtx)
	if err != nil {
		return 0, err
	}
	zone, err := ensureGuidedZone(wfCtx)
	if err != nil {
		return 0, err
	}
	current := paramNum(wfCtx.Params, "Gpu", 0)
	selected, opts := guidedGPUCountFormOptions(wfCtx, wfCtx.Result("查询可用配比"), gpuType, zone, current, wfCtx.Params, wfCtx.Result("查询GPU库存"))
	if selected == 0 || len(opts) == 0 {
		return 0, fmt.Errorf("%s 在 %s 暂无可选卡数量，请换一个可用区", gpuType, zone)
	}
	wfCtx.Params["Gpu"] = selected
	return selected, nil
}

func ensureGuidedCPUMemory(wfCtx *Context) (string, error) {
	gpuType, err := ensureGuidedGPUType(wfCtx)
	if err != nil {
		return "", err
	}
	zone, err := ensureGuidedZone(wfCtx)
	if err != nil {
		return "", err
	}
	gpu, err := ensureGuidedGPUCount(wfCtx)
	if err != nil {
		return "", err
	}
	current, opts := guidedCpuMemoryFormOptions(wfCtx, wfCtx.Result("查询可用配比"), gpuType, zone, gpu, wfCtx.Params, wfCtx.Result("查询GPU库存"))
	if current == "" || len(opts) == 0 {
		return "", fmt.Errorf("%s 在 %s 的 %.0f 卡暂无可选 CPU/内存规格，请换一个可用区或卡数量", gpuType, zone, gpu)
	}
	parsedZone, parsedGPU, cpu, memoryMB, err := parseGuidedSpecKey(current)
	if err != nil {
		return "", err
	}
	wfCtx.Params["Zone"] = parsedZone
	syncGuidedZoneMeta(wfCtx, parsedZone)
	wfCtx.Params["Gpu"] = parsedGPU
	wfCtx.Params["Cpu"] = cpu
	wfCtx.Params["Memory"] = memoryMB
	return current, nil
}

// markGuidedRecommendedOption tags the option matching value with a 推荐 badge so
// a model-driven recommendation reads as such on the card (the option is already
// the default selection). Prepends rather than overwrites the existing Note, which
// carries VRAM / 库存 detail.
func markGuidedRecommendedOption(opts []ConfirmFormOption, value string) {
	if value == "" {
		return
	}
	for i := range opts {
		if opts[i].Value == value {
			if opts[i].Note == "" {
				opts[i].Note = "推荐"
			} else {
				opts[i].Note = "推荐 · " + opts[i].Note
			}
			return
		}
	}
}
