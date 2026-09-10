package workflow

import (
	"fmt"
	"strings"
)

// The guided order is conditional: a step whose answer is already forced by
// the request or by what the catalog can sell is skipped, so the numbering a
// card shows is over the steps actually reached rather than over the full
// list. Each shouldSkip predicate answers for exactly one step.

// guidedStepLabel names this card's position for the confirmation payload. The
// wizard is conditional, so guidedStepPosition reports no total — the old
// "%d/%d" format outlived its denominator and rendered "4/0", a total of zero
// stated as fact. It uses the same vocabulary the card title does, so the
// payload and the card the user is reading cannot disagree.
func guidedStepLabel(wfCtx *Context, logical int) string {
	index, _ := guidedStepPosition(wfCtx, logical)
	return guidedOrdinal(index)
}

func guidedStepPosition(wfCtx *Context, logical int) (int, int) {
	order := guidedReachedOrder(wfCtx)
	for i, step := range order {
		if step == logical {
			return i + 1, 0
		}
	}
	// The wizard is conditional: choosing a source/image/GPU can remove later
	// cards, so its final total is unknowable at the first card. Expose a
	// monotonic ordinal and Total=0 (unknown) instead of renumbering prior cards.
	return len(order) + 1, 0
}

func guidedReachedOrder(wfCtx *Context) []int {
	if wfCtx == nil || wfCtx.Params == nil {
		return nil
	}
	raw := wfCtx.Params["GuidedReachedOrder"]
	var out []int
	switch values := raw.(type) {
	case []int:
		out = append(out, values...)
	case []any:
		for _, value := range values {
			switch n := value.(type) {
			case int:
				out = append(out, n)
			case float64:
				out = append(out, int(n))
			}
		}
	}
	return out
}

func guidedStepWasReached(wfCtx *Context, logical int) bool {
	for _, step := range guidedReachedOrder(wfCtx) {
		if step == logical {
			return true
		}
	}
	return false
}

// markGuidedStepReached records card order, which determines the displayed step
// ordinal.
func markGuidedStepReached(wfCtx *Context, logical int) {
	if wfCtx == nil || wfCtx.Params == nil {
		return
	}
	order := guidedReachedOrder(wfCtx)
	for _, step := range order {
		if step == logical {
			return
		}
	}
	wfCtx.Params["GuidedReachedOrder"] = append(order, logical)
}

func guidedStepTitle(index int, title string) string {
	return fmt.Sprintf("%s，%s", guidedOrdinal(index), title)
}

// guidedOrdinal names a card's position. The table covers 1..11 because that is
// the wizard's real ceiling once a multi-version family needs its own card — and a
// run that reached the digits mid-flow rendered "第五步" next to "第6步",
// which reads as two different numbering schemes rather than one sequence.
func guidedOrdinal(index int) string {
	numerals := []string{"一", "二", "三", "四", "五", "六", "七", "八", "九", "十", "十一"}
	if index >= 1 && index <= len(numerals) {
		return "第" + numerals[index-1] + "步"
	}
	return fmt.Sprintf("第%d步", index)
}

func shouldSkipGuidedGPUStep(wfCtx *Context) (bool, error) {
	current := paramStr(wfCtx.Params, "GpuType", "")
	if current == "" || !paramBool(wfCtx.Params, "GuidedGpuLocked", false) {
		return false, nil
	}
	supported := currentImageSupportedGPUs(wfCtx.Params, createImageResult(wfCtx))
	selected, opts := guidedGPUFormOptions(wfCtx, wfCtx.Result("查询可用配比"), supported,
		current, true, wfCtx.Params, wfCtx.Result("查询GPU库存"))
	// Prefilled values remain editable whenever the live catalog offers a choice.
	// An unavailable value must also reach the card that can explain or replace it.
	if !strings.EqualFold(selected, current) || !isOnlyEnabledOption(opts, current) {
		return false, nil
	}
	if len(supported) > 0 && containsFold(supported, current) && hasExplicitImageIntent(wfCtx.Params) &&
		initialParamSet(wfCtx, "Zone") && initialParamSet(wfCtx, "Gpu") &&
		initialParamSet(wfCtx, "Cpu") && initialParamSet(wfCtx, "Memory") {
		return true, nil
	}
	return false, nil
}

func shouldSkipGuidedZoneStep(wfCtx *Context) (bool, error) {
	current := paramStr(wfCtx.Params, "Zone", "")
	gpuType := paramStr(wfCtx.Params, "GpuType", "")
	if current == "" || gpuType == "" || !initialParamSet(wfCtx, "Zone") {
		return false, nil
	}
	selected, opts, _ := guidedZoneFormOptions(wfCtx, wfCtx.Result("查询可用配比"), gpuType, current, wfCtx.Params, wfCtx.Result("查询GPU库存"))
	return strings.EqualFold(selected, current) && isOnlyEnabledOption(opts, current), nil
}

func shouldSkipGuidedGPUCountStep(wfCtx *Context) (bool, error) {
	if !initialParamSet(wfCtx, "Gpu") {
		return false, nil
	}
	gpuType := paramStr(wfCtx.Params, "GpuType", "")
	zone := paramStr(wfCtx.Params, "Zone", "")
	current := paramNum(wfCtx.Params, "Gpu", 0)
	if gpuType == "" || zone == "" || current <= 0 {
		return false, nil
	}
	selected, opts := guidedGPUCountFormOptions(wfCtx, wfCtx.Result("查询可用配比"), gpuType, zone, current, wfCtx.Params, wfCtx.Result("查询GPU库存"))
	value := fmt.Sprintf("%.0f", current)
	return selected == current && isOnlyEnabledOption(opts, value), nil
}

func shouldSkipGuidedCPUMemoryStep(wfCtx *Context) (bool, error) {
	if !initialParamSet(wfCtx, "Cpu") {
		return false, nil
	}
	if !initialParamSet(wfCtx, "Memory") {
		return false, nil
	}
	gpuType := paramStr(wfCtx.Params, "GpuType", "")
	zone := paramStr(wfCtx.Params, "Zone", "")
	gpu := paramNum(wfCtx.Params, "Gpu", 0)
	cpu := paramNum(wfCtx.Params, "Cpu", 0)
	memoryMB := paramNum(wfCtx.Params, "Memory", 0)
	if gpuType == "" || zone == "" || gpu <= 0 || cpu <= 0 || memoryMB <= 0 {
		return false, nil
	}
	current := formatGuidedSpecKey(zone, gpu, cpu, memoryMB)
	selected, opts := guidedCpuMemoryFormOptions(wfCtx, wfCtx.Result("查询可用配比"), gpuType, zone, gpu, wfCtx.Params, wfCtx.Result("查询GPU库存"))
	return selected == current && isOnlyEnabledOption(opts, current), nil
}

// Image and source parameters come from the Agent or a form submission. They
// constrain selection; the final priced contract remains independently confirmed.
func shouldSkipGuidedImageSourceStep(wfCtx *Context) (bool, error) {
	if wfCtx == nil {
		return false, nil
	}
	// A source is a preselection, not a concrete image. Keep catalog browsing
	// available until an exact image or an actual source-card choice settles it.
	return strings.TrimSpace(paramStr(wfCtx.Params, "CompShareImageId", "")) != "" ||
		guidedStepWasReached(wfCtx, guidedStepImageSource), nil
}

func shouldSkipGuidedImageFacetsStep(wfCtx *Context) (bool, error) {
	if tenantImageInventorySelected(wfCtx) {
		return true, nil
	}
	if hasExplicitImageSelection(wfCtx.Params) {
		return true, nil
	}
	// No empty card: the facets step earns its place only when the chosen source's
	// catalog (this run's 查询镜像, refreshed by the re-query) offers a real ImageType
	// or 用途 choice. An absent facet never filters, so skipping here excludes
	// nothing. The tag facet is NOT consulted — it has its own card now, which is
	// reached whether or not this one is.
	set := createImageCandidates(wfCtx)
	return len(imageTypeFacetOptions(set)) == 0 &&
		len(imageCategoryFacetOptions(createImageTaxonomy(wfCtx), set)) == 0, nil
}

// shouldSkipGuidedImageTagStep drops the tag card whenever it has nothing real to
// ask. That is the normal case for community (the 用途 card already covered this
// axis at a stabler resolution) and for any candidate set whose remaining images
// carry no tags — which, after the type card, includes 系统镜像.
//
// imageTagFacetOptions counts over the post-type candidates, so "no tag left" here
// means exactly "no tag would have led anywhere". A one-option card (only 不限标签)
// is not a choice, so it is skipped too.
func shouldSkipGuidedImageTagStep(wfCtx *Context) (bool, error) {
	if tenantImageInventorySelected(wfCtx) {
		return true, nil
	}
	if hasExplicitImageSelection(wfCtx.Params) {
		return true, nil
	}
	set := createImageCandidates(wfCtx)
	if len(imageCategoryFacetOptions(createImageTaxonomy(wfCtx), set)) > 0 {
		return true, nil
	}
	return len(imageTagFacetOptions(set)) < 2, nil
}

// shouldSkipGuidedImageFamilyStep asks for a series only when browsing leaves a
// real choice BETWEEN families and at least one of them has more than one concrete
// version. A named image or Agent suggestion already gives the user a useful
// concrete-image picker, while flat platform rows are singleton families and retain
// the existing one-card flow.
func shouldSkipGuidedImageFamilyStep(wfCtx *Context) (bool, error) {
	if wfCtx == nil {
		return true, nil
	}
	if hasExplicitImageSelection(wfCtx.Params) {
		return true, nil
	}
	if strings.TrimSpace(paramStr(wfCtx.Params, "ImageFamily", "")) != "" ||
		strings.TrimSpace(paramStr(wfCtx.Params, "ImageName", "")) != "" ||
		strings.TrimSpace(paramStr(wfCtx.Params, "CompShareImageId", "")) != "" {
		return true, nil
	}
	families := createImageFamilies(wfCtx)
	if len(families) < 2 {
		return true, nil
	}
	for _, family := range families {
		if len(family.Variants) > 1 {
			return false, nil
		}
	}
	return true, nil
}

func shouldSkipGuidedImageStep(wfCtx *Context) (bool, error) {
	if wfCtx == nil || strings.TrimSpace(paramStr(wfCtx.Params, "CompShareImageId", "")) == "" {
		return false, nil
	}
	// An Agent-supplied image is preselected on the concrete-image card. Only a
	// choice already confirmed by this workflow can make that card redundant.
	return guidedStepWasReached(wfCtx, guidedStepImage) ||
		guidedStepWasReached(wfCtx, guidedStepImageFamily), nil
}

// tenantImageInventorySelected keeps tenant-scoped custom/shared catalogs out of
// the public image taxonomy. Their next card lists exactly the images visible to
// this account; an exact ID remains independently verified and confirmation-gated.
func tenantImageInventorySelected(wfCtx *Context) bool {
	if wfCtx == nil {
		return false
	}
	source := normalizedImageSource(paramStr(wfCtx.Params, "ImageSource", imageSourcePlatform))
	return source == imageSourceCustom || source == imageSourceSharing
}

func shouldSkipGuidedChargeTypeStep(wfCtx *Context) (bool, error) {
	if wfCtx == nil {
		return false, nil
	}
	// The supplied mode is a preselection, not a reason to remove the user's
	// purchase choice. A lone valid mode needs no separate selection card.
	return isOnlyEnabledOption(guidedChargeTypeOptions(wfCtx), createChargeType(wfCtx.Params)), nil
}

func stepGuidedChooseChargeType() Step {
	return Step{
		Name:              "选择计费方式",
		Type:              StepConfirm,
		SkipIf:            shouldSkipGuidedChargeTypeStep,
		BuildForm:         buildGuidedChargeTypeForm,
		ApplyOverrides:    applyGuidedChargeTypeOverrides,
		ConfirmSubmitMode: ConfirmSubmitContinue,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			return map[string]any{
				"workflow":   "CreateInstanceWorkflow",
				"step":       guidedStepLabel(wfCtx, guidedStepChargeType),
				"ChargeType": createChargeType(wfCtx.Params),
			}, nil
		},
	}
}

// chargeTypeChangeHint says where the purchase mode can be changed. The final
// card deliberately cannot change it — a late switch would desync the resource
// pool every earlier step queried against — so the honest answer depends on
// whether this run showed the purchase-mode card.
func chargeTypeChangeHint(wfCtx *Context) string {
	if guidedStepWasReached(wfCtx, guidedStepChargeType) {
		return "需要改用其他计费方式，请返回上面的「购买方式」一步重新选择。"
	}
	return "需要改用其他计费方式，请重新发起创建并说明要使用的计费方式。"
}

// guidedChargeTypeOptions is the charge-type card's option list. Unlike the
// plain card's createChargeTypeOptions it cannot name a zone — none is chosen
// yet — so it disables a mode only when nothing in the catalog sells it.
func guidedChargeTypeOptions(wfCtx *Context) []ConfirmFormOption {
	opts := make([]ConfirmFormOption, len(createFormChargeTypes))
	copy(opts, createFormChargeTypes)
	for i := range opts {
		if chargeTypeUnsupportedInCatalog(wfCtx, opts[i].Value) {
			opts[i].Disabled = true
			opts[i].Reason = "当前没有任何机型和可用区支持" + createInventoryPoolLabel(createInventoryPool(opts[i].Value)) + "购买方式"
			opts[i].Note = opts[i].Reason
		}
	}
	return opts
}
