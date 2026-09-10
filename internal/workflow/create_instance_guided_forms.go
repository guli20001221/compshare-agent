package workflow

import (
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/deployment"
)

// One builder per guided card. A form is a projection of the current params
// plus what the catalog and inventory currently allow; it never decides the
// order and never writes back. Disabled options stay visible with their
// reason, so a user can see why the thing they wanted is not offered.

func buildGuidedChargeTypeForm(wfCtx *Context) (*ConfirmForm, error) {
	opts := guidedChargeTypeOptions(wfCtx)
	current := createChargeType(wfCtx.Params)
	if !enabledOptionExists(opts, current) {
		current = firstEnabledValue(opts)
	}
	if current == "" {
		return nil, fmt.Errorf("暂无可用的计费方式")
	}
	index, total := guidedStepPosition(wfCtx, guidedStepChargeType)
	return &ConfirmForm{
		Version: 2,
		Step: &ConfirmFormStep{
			Index: index,
			Total: total,
			Title: guidedStepTitle(index, "请选择计费方式"),
			// Say what changes downstream, because it genuinely does: the GPU and
			// zone cards after this one are filtered by the mode chosen here.
			Description:    "计费方式决定后面能选哪些 GPU 和可用区：抢占式更便宜但可能被回收，且部分卡型和可用区只卖其中一种。按量付费适合先试跑，包日 / 包月适合长期占用。",
			PrimaryLabel:   "确认选择",
			SecondaryLabel: "跳过",
			Skippable:      true,
		},
		Fields: []ConfirmFormField{{
			Key: "ChargeType", Label: "计费方式", Type: "select",
			Value: current, Render: "cards", Editable: true, Options: opts,
		}},
	}, nil
}

func buildGuidedGPUForm(wfCtx *Context) (*ConfirmForm, error) {
	gpuType := paramStr(wfCtx.Params, "GpuType", "")
	if gpuType == "" {
		selected, err := ensureGuidedGPUType(wfCtx)
		if err != nil {
			return nil, err
		}
		gpuType = selected
	}
	supported := currentImageSupportedGPUs(wfCtx.Params, createImageResult(wfCtx))
	locked := paramBool(wfCtx.Params, "GuidedGpuLocked", false) && gpuType != ""
	recommended := paramBool(wfCtx.Params, "GuidedRecommended", false) && gpuType != ""
	selected, opts := guidedGPUFormOptions(wfCtx, wfCtx.Result("查询可用配比"), supported, gpuType, locked, wfCtx.Params, wfCtx.Result("查询GPU库存"))
	if len(opts) == 0 {
		return nil, fmt.Errorf("暂无可选 GPU 型号")
	}
	lockedUnavailable := locked && !enabledOptionExists(opts, gpuType)
	if selected == "" {
		return nil, fmt.Errorf("暂无有库存的 GPU 型号，请换一个型号或稍后再试")
	}
	index, total := guidedStepPosition(wfCtx, guidedStepGPU)
	title := guidedStepTitle(index, "请选择 GPU 参数")
	desc := "GPU 型号决定可用显存与算力：显存越大，越能支撑更大的模型与更高的批量。不确定时可先用默认项。"
	if lockedUnavailable {
		title = guidedStepTitle(index, "原配置当前不可用，请重新选择 GPU")
		desc = fmt.Sprintf("原配置中的 %s 与当前镜像、计费方式或库存条件不匹配。请选择下方可用型号；系统会重新检查可用区、卡数、CPU/内存、库存和价格，并再次请你确认。", gpuType)
	} else if locked {
		title = guidedStepTitle(index, "请确认 GPU 参数")
		desc = "已按你的需求推荐合适的 GPU 显存规格，可直接确认，也可在下方调整。显存越大，可支撑的模型与批量越大。"
	} else if recommended {
		// A model-driven deploy keeps every GPU on the card but pre-selects the
		// matcher's pick; flag it so the recommendation is visible, not just default.
		title = guidedStepTitle(index, "请确认推荐的 GPU 参数")
		desc = "已根据你要部署的模型推荐合适的 GPU 显存规格（默认已选中），如需更大显存可在下方调整。"
		markGuidedRecommendedOption(opts, selected)
	}
	return &ConfirmForm{
		Version: 2,
		Step: &ConfirmFormStep{
			Index:          index,
			Total:          total,
			Title:          title,
			Description:    desc,
			PrimaryLabel:   "确认选择",
			SecondaryLabel: "跳过",
			Skippable:      true,
		},
		Fields: []ConfirmFormField{{
			Key: "GpuType", Label: "GPU 参数", Type: "select",
			Value: selected, Render: "cards", Editable: true, Options: opts,
		}},
	}, nil
}

func buildGuidedZoneForm(wfCtx *Context) (*ConfirmForm, error) {
	gpuType, err := ensureGuidedGPUType(wfCtx)
	if err != nil {
		return nil, err
	}
	current, err := ensureGuidedZone(wfCtx)
	if err != nil {
		return nil, err
	}
	_, opts, stoodDown := guidedZoneFormOptions(wfCtx, wfCtx.Result("查询可用配比"), gpuType, current, wfCtx.Params, wfCtx.Result("查询GPU库存"))
	if len(opts) == 0 {
		return nil, fmt.Errorf("%s 暂无可选可用区，请换一个 GPU 型号或稍后再试", gpuType)
	}
	description := "可用区影响 GPU 现货与就近接入。建议优先选择有现货的可用区；同一型号在不同区的库存可能不同。"
	if stoodDown {
		description = guidedZoneStandDownDescription()
	}
	index, total := guidedStepPosition(wfCtx, guidedStepZone)
	return &ConfirmForm{
		Version: 2,
		Step: &ConfirmFormStep{
			Index:          index,
			Total:          total,
			Title:          guidedStepTitle(index, "请选择可用区"),
			Description:    description,
			PrimaryLabel:   "确认选择",
			SecondaryLabel: "跳过",
			Skippable:      true,
		},
		Fields: []ConfirmFormField{{
			Key: "Zone", Label: "可用区", Type: "select",
			Value: current, Render: "cards", Editable: true, Options: opts,
		}},
	}, nil
}

func buildGuidedGPUCountForm(wfCtx *Context) (*ConfirmForm, error) {
	gpuType, err := ensureGuidedGPUType(wfCtx)
	if err != nil {
		return nil, err
	}
	zone, err := ensureGuidedZone(wfCtx)
	if err != nil {
		return nil, err
	}
	gpu, err := ensureGuidedGPUCount(wfCtx)
	if err != nil {
		return nil, err
	}
	_, opts := guidedGPUCountFormOptions(wfCtx, wfCtx.Result("查询可用配比"), gpuType, zone, gpu, wfCtx.Params, wfCtx.Result("查询GPU库存"))
	if len(opts) == 0 {
		return nil, fmt.Errorf("%s 在 %s 暂无可选卡数量，请换一个可用区", gpuType, zone)
	}
	current := fmt.Sprintf("%.0f", gpu)
	index, total := guidedStepPosition(wfCtx, guidedStepGPUCount)
	return &ConfirmForm{
		Version: 2,
		Step: &ConfirmFormStep{
			Index:          index,
			Total:          total,
			Title:          guidedStepTitle(index, "请选择卡数量"),
			Description:    "卡数量越多，显存与并行算力越大，费用也相应增加。常规推理通常单卡即可，大模型或分布式训练再增加卡数。",
			PrimaryLabel:   "确认选择",
			SecondaryLabel: "跳过",
			Skippable:      true,
		},
		Fields: []ConfirmFormField{{
			Key: "Gpu", Label: "卡数量", Type: "select",
			Value: current, Render: "cards", Editable: true, Options: opts,
		}},
	}, nil
}

func buildGuidedCpuMemoryForm(wfCtx *Context) (*ConfirmForm, error) {
	gpuType, err := ensureGuidedGPUType(wfCtx)
	if err != nil {
		return nil, err
	}
	zone, err := ensureGuidedZone(wfCtx)
	if err != nil {
		return nil, err
	}
	gpu, err := ensureGuidedGPUCount(wfCtx)
	if err != nil {
		return nil, err
	}
	current, err := ensureGuidedCPUMemory(wfCtx)
	if err != nil {
		return nil, err
	}
	_, opts := guidedCpuMemoryFormOptions(wfCtx, wfCtx.Result("查询可用配比"), gpuType, zone, gpu, wfCtx.Params, wfCtx.Result("查询GPU库存"))
	if len(opts) == 0 {
		return nil, fmt.Errorf("%s 在 %s 的 %.0f 卡暂无可选 CPU/内存规格，请换一个可用区或卡数量", gpuType, zone, gpu)
	}
	index, total := guidedStepPosition(wfCtx, guidedStepCPUMemory)
	return &ConfirmForm{
		Version: 2,
		Step: &ConfirmFormStep{
			Index:          index,
			Total:          total,
			Title:          guidedStepTitle(index, "请选择 CPU/内存"),
			Description:    "CPU 与内存随 GPU 套餐配比，默认规格已匹配所选 GPU。数据预处理重、多进程加载较多时可选更高配比。",
			PrimaryLabel:   "确认选择",
			SecondaryLabel: "跳过",
			Skippable:      true,
		},
		Fields: []ConfirmFormField{{
			Key: "CpuMemory", Label: "CPU/内存", Type: "select",
			Value: current, Render: "cards", Editable: true, Options: opts,
		}},
	}, nil
}

// imageSourceFacetOptions lists the image sources the create flow supports. Each
// value selects a concrete upstream catalog.
//
// These values are a real choice of where to look, not a local category: platform
// and custom use flat ImageSet[] results, while community publishes grouped
// CompshareImageGroup[].Data[] versions. The form keeps that source boundary
// explicit before later cards rank only the chosen live catalog.
//
// Order and default are deliberately unchanged: platform stays first and stays the
// default, so this reframes the question without silently moving anyone to a
// different catalog.
func imageSourceFacetOptions() []ConfirmFormOption {
	return []ConfirmFormOption{
		{
			Value: imageSourcePlatform, Label: "平台镜像",
			Note: "平台官方镜像：干净的系统镜像，或预装 PyTorch / TensorFlow 等框架的基础镜像",
		},
		{
			Value: imageSourceCommunity, Label: "社区镜像",
			Note: "社区镜像：开箱即用的应用与模型，可按数字人、图像视频生成、语音、LLM 等用途挑选",
		},
		{
			Value: imageSourceCustom, Label: "自制镜像",
			Note: "自制镜像：仅查看当前账户制作的镜像，可直接用于创建实例",
		},
		{
			Value: imageSourceSharing, Label: "共享镜像",
			Note: "共享镜像：仅查看其他账户共享给当前账户、当前可见的镜像，可直接用于创建实例",
		},
	}
}

// imageTypeFacetOptions returns the distinct real ImageType values among the current
// candidates, in catalog order. It is a REAL facet: the options come straight from
// each candidate's ImageType field, never from a purpose keyword table. Returns nil
// (facet omitted) when fewer than two types are present — a single-type list is no
// choice.
// It carries the same "N 个镜像" count the 用途 facet does. The two facets are the
// second step of opposite branches of the same card — 自己搭环境 gets types,
// 跑现成的应用 gets 用途 — and one of them silently lacking counts reads as
// unfinished rather than as a different kind of filter.
func imageTypeFacetOptions(set imageCandidateSet) []ConfirmFormOption {
	order := []string{}
	count := map[string]int{}
	label := map[string]string{}
	familiesByType := map[string]map[string]bool{}
	entries := candidateEntries(set.snap, set.base)
	grouped := imageCandidatesGroupIntoFamilies(entries)
	for _, e := range entries {
		t := strings.TrimSpace(e.ImageType)
		if t == "" {
			continue
		}
		key := strings.ToLower(t)
		if _, seen := count[key]; !seen {
			order = append(order, key)
			label[key] = t
			familiesByType[key] = map[string]bool{}
		}
		familyKey := e.FamilyKey()
		if !familiesByType[key][familyKey] {
			familiesByType[key][familyKey] = true
			count[key]++
		}
	}
	if len(order) < 2 {
		return nil
	}
	opts := []ConfirmFormOption{{Value: "", Label: "全部类型"}}
	for _, key := range order {
		opts = append(opts, ConfirmFormOption{
			Value: label[key],
			Label: imageTypeFacetLabel(label[key]),
			Note:  imageFamilyCountNote(count[key], grouped),
		})
	}
	return opts
}

// imageTagFacetOptions returns the distinct real Tags among the candidates the
// TYPE step left behind (set.afterType), each with the count of candidates that
// actually carry it. The values are REAL catalog tags (镜像标签), never synthesized
// purpose keys, so a tag membership filter is exact — and no alias table maps
// miniconda onto Miniconda3, because inventing that equivalence is exactly the
// keyword-table this repo refuses to grow. Two near-identical upstream tags stay
// two options; their counts now say which one has images behind it.
//
// Counting over afterType rather than the whole catalog is what makes every offered
// tag reachable: a tag whose only images the type already excluded scores 0 and is
// not offered, so the card can no longer produce an empty picker.
//
// Returns nil (facet OMITTED — never a default, never a blocker) when no candidate
// carries a tag: an absent tag facet must never exclude any image.
func imageTagFacetOptions(set imageCandidateSet) []ConfirmFormOption {
	order := []string{}
	count := map[string]int{}
	label := map[string]string{}
	familiesByTag := map[string]map[string]bool{}
	entries := candidateEntries(set.snap, set.afterType)
	grouped := imageCandidatesGroupIntoFamilies(entries)
	for _, e := range entries {
		for _, tag := range e.Tags {
			t := strings.TrimSpace(tag)
			if t == "" {
				continue
			}
			key := strings.ToLower(t)
			if _, seen := count[key]; !seen {
				order = append(order, key)
				label[key] = t
				familiesByTag[key] = map[string]bool{}
			}
			familyKey := e.FamilyKey()
			if !familiesByTag[key][familyKey] {
				familiesByTag[key][familyKey] = true
				count[key]++
			}
		}
	}
	if len(order) == 0 {
		return nil
	}
	opts := []ConfirmFormOption{{Value: "", Label: "不限标签"}}
	for _, key := range order {
		opts = append(opts, ConfirmFormOption{
			Value: label[key],
			Label: label[key],
			Note:  imageFamilyCountNote(count[key], grouped),
		})
	}
	return opts
}

// candidateEntries resolves a candidate list back to its catalog rows. A selection
// whose id is absent from the snapshot (an externally-threaded image) carries no
// type or tags to count, so it is skipped rather than counted as an unknown.
func candidateEntries(snap *deployment.ImageCatalogSnapshot, candidates []deployment.ImageSelection) []deployment.ImageCatalogEntry {
	out := make([]deployment.ImageCatalogEntry, 0, len(candidates))
	for _, sel := range candidates {
		if entry, ok := snap.ByID(sel.ID); ok {
			out = append(out, entry)
		}
	}
	return out
}

// imageCategoryFacetOptions offers the platform's 用途 categories, restricted to
// the ones that actually contain an image in THIS catalog.
//
// It replaces the flat tag facet when it is available. The flat facet listed the
// raw tag of every row on the page, so what the user could filter by depended on
// which rows came back — and the labels were the tag strings themselves, including
// the compound ones upstream stores. The categories are the platform's own, stable
// across pages, and there are 7 of them rather than dozens.
//
// An empty category is never offered: a filter that can only produce an empty list
// is worse than no filter. Fewer than two usable categories means there is no
// choice to make, so the facet is omitted entirely rather than shown with one
// option — the same rule imageTypeFacetOptions already follows.
func imageCategoryFacetOptions(taxonomy *deployment.ImageTaxonomy, set imageCandidateSet) []ConfirmFormOption {
	if !taxonomy.Available() || set.snap == nil {
		return nil
	}
	count := map[string]int{}
	familiesByCategory := map[string]map[string]bool{}
	entries := candidateEntries(set.snap, set.base)
	grouped := imageCandidatesGroupIntoFamilies(entries)
	for _, e := range entries {
		for _, c := range taxonomy.CategoriesOf(e.Tags) {
			if familiesByCategory[c] == nil {
				familiesByCategory[c] = map[string]bool{}
			}
			familyKey := e.FamilyKey()
			if !familiesByCategory[c][familyKey] {
				familiesByCategory[c][familyKey] = true
				count[c]++
			}
		}
	}
	var opts []ConfirmFormOption
	for _, c := range taxonomy.Categories() {
		n := count[c]
		if n == 0 {
			continue
		}
		opts = append(opts, ConfirmFormOption{
			Value: c, Label: c, Note: imageFamilyCountNote(n, grouped),
		})
	}
	if len(opts) < 2 {
		return nil
	}
	return append([]ConfirmFormOption{{Value: "", Label: "全部用途"}}, opts...)
}

// imageTypeFacetLabel names an upstream ImageType for the card.
//
// Known upstream values are localized. An unknown value is shown verbatim rather
// than guessed at.
func imageTypeFacetLabel(t string) string {
	switch strings.ToLower(t) {
	case "system":
		return "系统镜像"
	case "app":
		return "框架 / 应用镜像"
	case "game":
		return "游戏镜像"
	case "other":
		return "其他镜像"
	case "custom":
		return "自制镜像"
	case "community":
		return "社区镜像"
	default:
		return t
	}
}

// filterImagesByFacets narrows a ranked selection list by the optional ImageType and
// ImageTag facets. A facet filters ONLY when explicitly set: an empty facet is "no
// filter", NEVER "match nothing" — so an unset tag never excludes an image, and an
// image with no Tags is dropped only when a tag WAS asked for (it genuinely lacks
// it). Membership is exact against the real catalog Tags — no keyword table.
func filterImagesByFacets(snap *deployment.ImageCatalogSnapshot, ranked []deployment.ImageSelection, params map[string]any, taxonomy *deployment.ImageTaxonomy) []deployment.ImageSelection {
	wantType := strings.TrimSpace(paramStr(params, "ImageType", ""))
	wantTag := strings.TrimSpace(paramStr(params, "ImageTag", ""))
	wantCategory := strings.TrimSpace(paramStr(params, "ImageCategory", ""))
	wantFamily := strings.TrimSpace(paramStr(params, "ImageFamily", ""))
	if wantType == "" && wantTag == "" && wantCategory == "" && wantFamily == "" {
		return ranked
	}
	out := make([]deployment.ImageSelection, 0, len(ranked))
	for _, sel := range ranked {
		if imageSelectionMatchesFacets(snap, sel.ID, wantType, wantTag) &&
			imageSelectionMatchesCategory(snap, taxonomy, sel.ID, wantCategory) &&
			imageSelectionMatchesFamily(snap, sel.ID, wantFamily) {
			out = append(out, sel)
		}
	}
	return out
}

// imageSelectionMatchesCategory reports whether one image belongs to the selected
// 用途 category. An unset category is no filter.
//
// A category the taxonomy cannot resolve (fetch failed, unknown name) also does
// NOT filter: the alternative is excluding every image on the strength of a
// classification we could not read, which turns a degraded read into an empty
// picker. Absence of evidence is not evidence of a mismatch — the same rule the
// tag facet follows for untagged images.
func imageSelectionMatchesCategory(snap *deployment.ImageCatalogSnapshot, taxonomy *deployment.ImageTaxonomy, id, wantCategory string) bool {
	if wantCategory == "" || !taxonomy.Available() {
		return true
	}
	entry, ok := snap.ByID(id)
	if !ok {
		return false
	}
	return containsFold(taxonomy.CategoriesOf(entry.Tags), wantCategory)
}

// imageCandidatesGroupIntoFamilies reports whether the rows a card counted actually
// form families — at least one family holding more than one concrete version.
//
// Every counted row belongs to some family, so a family count is always computable;
// that is not the same as the catalog HAVING families. A source that publishes no
// family relation gets one singleton family per image (FamilyKey falls back to the
// image id), and there the family count IS the image count — naming it 系列 would
// describe a hierarchy the source does not have, and promise a family card that
// shouldSkipGuidedImageFamilyStep will skip.
//
// Decided per card, from the same rows that card counted, so the noun cannot
// disagree with the number beside it.
func imageCandidatesGroupIntoFamilies(entries []deployment.ImageCatalogEntry) bool {
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		key := e.FamilyKey()
		if seen[key] {
			return true
		}
		seen[key] = true
	}
	return false
}

// imageCountNoun names what a facet count counts.
func imageCountNoun(grouped bool) string {
	if grouped {
		return "镜像系列"
	}
	return "镜像"
}

func imageFamilyCountNote(n int, grouped bool) string {
	return fmt.Sprintf("%d 个%s", n, imageCountNoun(grouped))
}

// imageSelectionMatchesFamily applies a previously-confirmed family choice. The
// empty value is deliberately unconstrained, matching the other optional facets.
func imageSelectionMatchesFamily(snap *deployment.ImageCatalogSnapshot, id, wantFamily string) bool {
	if wantFamily == "" {
		return true
	}
	entry, ok := snap.ByID(id)
	return ok && entry.FamilyKey() == wantFamily
}

// imageSelectionMatchesFacets reports whether one image id satisfies the explicitly
// set ImageType / ImageTag facets. With NO facet set it is unconstrained and returns
// true even for an id absent from this snapshot (an externally-threaded selection is
// still honored) — a facet only ever constrains when the user actually picked one.
// Under an active facet, an id we cannot verify against the catalog is dropped, and a
// set tag is exact membership against the image's real Tags.
func imageSelectionMatchesFacets(snap *deployment.ImageCatalogSnapshot, id, wantType, wantTag string) bool {
	if wantType == "" && wantTag == "" {
		return true
	}
	entry, ok := snap.ByID(id)
	if !ok {
		return false
	}
	if wantType != "" && !strings.EqualFold(strings.TrimSpace(entry.ImageType), wantType) {
		return false
	}
	if wantTag != "" && !containsFold(entry.Tags, wantTag) {
		return false
	}
	return true
}

// buildGuidedImageFacetsForm is the SECOND of the staged image flow: it offers ONE
// narrowing axis — the ImageType facet, or the 用途 category when the platform's own
// classification covers this catalog — built ONLY from the catalog of the source
// chosen in the prior source step (createImageCatalog reads this run's 查询镜像,
// which the re-query refreshed to the chosen source). The source itself is NOT
// editable here — that is the separate source step, so the facets shown are always
// the chosen source's real types, never a foreign source's. Natural-language intent
// ("大模型推理" / "深度学习") is NOT handled here — the central Agent maps it to a real
// image before the workflow runs; this step is the click-through fallback.
//
// ImageTag has a later card whose options are computed from these selections.
func buildGuidedImageFacetsForm(wfCtx *Context) (*ConfirmForm, error) {
	set := createImageCandidates(wfCtx)
	grouped := imageCandidatesGroupIntoFamilies(candidateEntries(set.snap, set.base))
	index, total := guidedStepPosition(wfCtx, guidedStepImageFacets)
	var fields []ConfirmFormField
	if opts := imageTypeFacetOptions(set); len(opts) > 0 {
		fields = append(fields, ConfirmFormField{
			Key: "ImageType", Label: "镜像类型", Type: "select",
			Value: paramStr(wfCtx.Params, "ImageType", ""), Editable: true, Options: opts,
		})
	}
	// 用途 supersedes the raw tag list when the platform's classification is
	// available. They are the same axis at two resolutions, and the category is the
	// stable one, so a catalog it covers never reaches the tag card at all.
	categoryOpts := imageCategoryFacetOptions(createImageTaxonomy(wfCtx), set)
	if len(categoryOpts) > 0 {
		fields = append(fields, ConfirmFormField{
			Key: "ImageCategory", Label: "用途", Type: "select",
			Value: paramStr(wfCtx.Params, "ImageCategory", ""), Editable: true, Options: categoryOpts,
		})
	}
	// Title and copy follow whichever facet this branch actually offers, so the card
	// reads as the second half of the question the first card asked rather than as a
	// generic "筛选" step that happens to show different fields.
	// The noun and the promised next card both follow whether THIS catalog groups.
	// A flat source skips the family card, so telling a platform user the next step
	// shows 镜像系列 would name a card that will not appear.
	noun := imageCountNoun(grouped)
	nextStep := "选择后下一步只展示匹配的真实镜像。"
	if grouped {
		nextStep = "选择后下一步只展示匹配的镜像系列，并在需要时选择版本。"
	}
	title := "缩小镜像范围"
	description := "镜像类型来自所选目录里的真实镜像。留空表示不按它筛选，不会排除任何镜像。" + nextStep
	switch {
	case len(categoryOpts) > 0:
		title = "想跑哪一类"
		description = fmt.Sprintf("用途分类来自平台自己的镜像分类目录，每项后的数量是当前目录里真实匹配的%s数。留空表示不按用途筛选，不会排除任何镜像。%s", noun, nextStep)
	case len(fields) > 0 && fields[0].Key == "ImageType":
		title = "要哪种底座"
		description = fmt.Sprintf("系统镜像是干净的操作系统，框架 / 应用镜像预装了 PyTorch、TensorFlow 等环境。每项后的数量是目录里真实匹配的%s数，留空表示不筛选。", noun)
	}
	return &ConfirmForm{
		Version: 2,
		Step: &ConfirmFormStep{
			Index:          index,
			Total:          total,
			Title:          guidedStepTitle(index, title),
			Description:    description,
			PrimaryLabel:   "确认选择",
			SecondaryLabel: "跳过",
			Skippable:      true,
		},
		Fields: fields,
	}, nil
}

// buildGuidedImageTagForm asks the raw-tag question on its own card, AFTER the type
// card, so its options describe the candidates the type actually left. Every tag
// offered here has at least one image behind it and the count says how many — which
// is what makes "系统镜像 + pytorch" unreachable rather than a dead end the user was
// invited to click.
//
// No alias table: miniconda and Miniconda3 are two upstream tags and stay two
// options. Deciding they mean the same thing is a keyword table, and the counts
// already tell the user which one has images.
func buildGuidedImageTagForm(wfCtx *Context) (*ConfirmForm, error) {
	set := createImageCandidates(wfCtx)
	opts := imageTagFacetOptions(set)
	if len(opts) == 0 {
		return nil, fmt.Errorf("当前候选镜像没有可用标签")
	}
	noun := imageCountNoun(imageCandidatesGroupIntoFamilies(candidateEntries(set.snap, set.afterType)))
	index, total := guidedStepPosition(wfCtx, guidedStepImageTag)
	return &ConfirmForm{
		Version: 2,
		Step: &ConfirmFormStep{
			Index:          index,
			Total:          total,
			Title:          guidedStepTitle(index, "再按标签缩小范围"),
			Description:    fmt.Sprintf("标签是所选目录里镜像自带的原始标签，每项后的数量是当前候选里真实带该标签的%s数。留空表示不按标签筛选，不会排除任何镜像。", noun),
			PrimaryLabel:   "确认选择",
			SecondaryLabel: "跳过",
			Skippable:      true,
		},
		Fields: []ConfirmFormField{{
			Key: "ImageTag", Label: "镜像标签", Type: "select",
			Value: paramStr(wfCtx.Params, "ImageTag", ""), Editable: true, Options: opts,
		}},
	}, nil
}

// buildGuidedImageSourceForm is the first card of the image flow. It asks what the
// user wants to do and stores the answer as ImageSource, so the following re-query
// and filter step rebuild from the matching catalog.
//
// The card no longer asks "哪个来源" — see imageSourceFacetOptions. Each branch then
// gets the filter that fits its data, with no branch-specific code: community rows
// all carry ImageType=Community so the type facet omits itself for lack of a
// choice, and platform tags barely intersect the platform's 用途 classification so
// the category facet omits itself the same way. The two filters select themselves.
func buildGuidedImageSourceForm(wfCtx *Context) (*ConfirmForm, error) {
	index, total := guidedStepPosition(wfCtx, guidedStepImageSource)
	source := normalizedImageSource(paramStr(wfCtx.Params, "ImageSource", imageSourcePlatform))
	return &ConfirmForm{
		Version: 2,
		Step: &ConfirmFormStep{
			Index:          index,
			Total:          total,
			Title:          guidedStepTitle(index, "你想怎么开始"),
			Description:    "先说清楚要做什么，下一步只在对应的真实镜像目录里筛选和挑选。改这一步会按新目录重新查询，并展示它真实的分类与镜像。",
			PrimaryLabel:   "确认选择",
			SecondaryLabel: "跳过",
			Skippable:      true,
		},
		Fields: []ConfirmFormField{{
			Key: "ImageSource", Label: "使用方式", Type: "select",
			Value: source, Render: "cards", Editable: true, Options: imageSourceFacetOptions(),
		}},
	}, nil
}

// guidedImagePageDescription states what this card is showing and, when the
// candidate list is longer than the page, the total candidate count and how to
// narrow it.
func guidedImagePageDescription(shown, candidates int) string {
	const base = "先确定实际创建使用的镜像，后续 GPU、可用区和库存检查都以这一个镜像 ID 为准。"
	if candidates <= shown {
		return base
	}
	return fmt.Sprintf("%s当前展示 %d 个，共 %d 个匹配镜像；如果这里没有想要的，回上一步改类型或标签，或直接告诉我镜像名称。",
		base, shown, candidates)
}

func guidedImageFamilyPageDescription(shown, families int) string {
	const base = "先选择想使用的镜像系列；若该系列有多个可用版本，下一步再确认具体版本。"
	if families <= shown {
		return base
	}
	return fmt.Sprintf("%s当前展示 %d 个，共 %d 个匹配镜像系列；如果这里没有想要的，可回上一步调整筛选，或直接告诉我镜像名称。",
		base, shown, families)
}

// buildGuidedImageFamilyForm keeps a catalog's natural hierarchy visible: users
// choose a recognisable image family first, then a concrete version only when that
// family actually has a version choice. The same model represents flat sources as
// singleton families, which skip this card altogether.
func buildGuidedImageFamilyForm(wfCtx *Context) (*ConfirmForm, error) {
	current, opts, families := guidedImageFamilyFormOptionsForContext(wfCtx)
	if len(opts) == 0 {
		return nil, fmt.Errorf("未找到可选镜像系列，请换一个镜像来源或稍后再试")
	}
	if current == "" {
		current = opts[0].Value
	}
	index, total := guidedStepPosition(wfCtx, guidedStepImageFamily)
	return &ConfirmForm{
		Version: 2,
		Step: &ConfirmFormStep{
			Index:          index,
			Total:          total,
			Title:          guidedStepTitle(index, "请选择镜像系列"),
			Description:    guidedImageFamilyPageDescription(len(opts), families),
			PrimaryLabel:   "确认选择",
			SecondaryLabel: "取消",
		},
		Fields: []ConfirmFormField{{
			Key: "ImageFamily", Label: "镜像系列", Type: "select",
			Value: current, Render: "cards", Editable: true, Options: opts,
		}},
	}, nil
}

func guidedImageVersionPageDescription(family deployment.ImageFamily, shown, candidates int) string {
	base := fmt.Sprintf("已选择「%s」。请确认实际创建使用的具体版本，后续 GPU、可用区和库存检查都以这个镜像 ID 为准。", family.Name)
	if candidates <= shown {
		return base
	}
	return fmt.Sprintf("%s当前展示 %d 个，共 %d 个可用版本。", base, shown, candidates)
}

func buildGuidedImageForm(wfCtx *Context) (*ConfirmForm, error) {
	gpuType := paramStr(wfCtx.Params, "GpuType", "")
	current, opts, candidates := guidedImageFormOptionsForContext(wfCtx, gpuType)
	if len(opts) == 0 {
		return nil, fmt.Errorf("未找到可选镜像，请换一个镜像来源或稍后再试")
	}
	if current == "" {
		current = opts[0].Value
	}
	index, total := guidedStepPosition(wfCtx, guidedStepImage)
	title, description, fieldLabel := "请选择具体镜像", guidedImagePageDescription(len(opts), candidates), "镜像"
	if family, ok := selectedImageFamily(wfCtx); ok && len(family.Variants) > 1 {
		title = "请选择具体版本"
		description = guidedImageVersionPageDescription(family, len(opts), candidates)
		fieldLabel = "版本"
	}
	return &ConfirmForm{
		Version: 2,
		Step: &ConfirmFormStep{
			Index:          index,
			Total:          total,
			Title:          guidedStepTitle(index, title),
			Description:    description,
			PrimaryLabel:   "确认选择",
			SecondaryLabel: "取消",
		},
		Fields: []ConfirmFormField{{
			Key: "ImageId", Label: fieldLabel, Type: "select",
			Value: current, Render: "cards", Editable: true, Options: opts,
		}},
	}, nil
}

func buildGuidedFinalForm(wfCtx *Context) (*ConfirmForm, error) {
	_, _, _, _, err := resolveTargetSpec(wfCtx)
	if err != nil {
		return nil, err
	}
	gpuType, _ := wfCtx.Params["GpuType"].(string)

	var fields []ConfirmFormField
	// The concrete image was settled before any capacity card. The final card only
	// states that decision: changing it here would invalidate GPU/zone/spec choices
	// that were computed for a different image. A future "修改镜像" affordance must
	// return to the image step and re-run the dependency chain, not edit in place.
	if cur, opts, _ := guidedImageFormOptionsForContext(wfCtx, gpuType); cur != "" && len(opts) > 0 {
		fields = append(fields, ConfirmFormField{
			Key: "ImageId", Label: "镜像", Type: "select", Value: cur, Editable: false,
		})
	}
	// ChargeType is stated, not offered, on THIS card: its edit re-runs only from
	// 形成执行草稿, while the GPU card, the zone card, the per-zone capacity probe
	// and the spec capacity check are all pool-scoped and have already run.
	// Switching to Spot at the end left every card the user had accepted
	// describing the on-demand pool, and only 检查库存 re-checked — which is why a
	// Spot create surfaced as a plain 库存不足 on a spec the cards had shown as
	// available. It is asked earlier instead, on its own card
	// (guidedStepChargeType), where changing it still re-runs everything it scopes.
	index, total := guidedStepPosition(wfCtx, guidedStepFinal)
	return &ConfirmForm{
		Version: 2,
		Step: &ConfirmFormStep{
			Index: index,
			Total: total,
			Title: guidedStepTitle(index, "确认镜像与计费"),
			// The final summary states the purchase choice and its price. Changes
			// belong to the earlier card, before billing-scoped capacity checks.
			Description: fmt.Sprintf(
				"镜像决定开机即用的预装环境（框架与驱动）。本次确认只创建实例及所选镜像；镜像已包含的软件会随实例提供，其他软件不会自动安装。当前计费方式为「%s」，价格按此计算；%s确认无误后点击下方按钮即开始创建。",
				chargeTypeLabel(createChargeType(wfCtx.Params)),
				chargeTypeChangeHint(wfCtx)),
			PrimaryLabel:   "确认部署",
			SecondaryLabel: "取消",
			Final:          true,
		},
		Fields: fields,
	}, nil
}
