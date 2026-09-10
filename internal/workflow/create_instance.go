package workflow

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/compshare-agent/internal/deployment"
)

// Guided wizard step order. Image is resolved BEFORE the hardware specs so that
// (1) the GPU list can be constrained to the chosen image's SupportedGpuTypes and
// (2) the 卡数量 / CPU-内存 options can be gated by a CheckCompShareResourceCapacity
// call that requires the real image. When the image is already known (recommended
// or explicitly named) the image steps skip and the flow starts at GPU. The
// numeric values are wizard order only; guidedStepPosition derives the visible
// "第N步" index from this order, so reordering here reorders the card.
const (
	guidedStepImageSource = iota + 1
	guidedStepImageFacets
	// The tag question is its OWN card, asked after the type card, because the two
	// are not independent: platform System images carry no tags at all (0/9 live),
	// so 系统镜像 + any 标签 ANDs to an empty picker. Asking them on one card that
	// submits once offered the user a pair that could only dead-end. Split, the tag
	// options are computed from the candidates the type left behind — and when that
	// leaves no tag worth asking about, this card skips itself.
	guidedStepImageTag
	// A source may publish several concrete versions of one recognizable image
	// family. The family is chosen before a concrete version so a category browse
	// never fills the first screen with one series' versions. Flat sources simply
	// produce singleton families and skip this step.
	guidedStepImageFamily
	guidedStepImage
	// Charge type sits between the image and the GPU because that is the only
	// window where it is both askable and still early enough. Everything from the
	// GPU card on is pool-scoped (the GPU and zone cards gate on purchase mode,
	// the zone capacity probe and the spec check send ChargeType), so it must
	// precede them. It could not be asked EARLIER than 查询可用配比 — guidedStepPosition
	// decides which later cards are skippable by reading that catalog, so a card
	// before it reports a step count computed as if nothing downstream is skipped.
	// The window exists at all only because two things turned out not to be
	// charge-scoped after measurement: the catalog query itself (InstanceType=spot
	// returns an empty catalog — see stepQueryInstanceTypes) and the inventory
	// snapshot (it carries BOTH pools, so it is fetched without a charge type).
	guidedStepChargeType
	guidedStepGPU
	guidedStepZone
	guidedStepGPUCount
	guidedStepCPUMemory
	guidedStepFinal
	guidedStepFirst = guidedStepImageSource
)

const (
	imageSourcePlatform  = "platform"
	imageSourceCommunity = "community"
	imageSourceCustom    = "custom"
	imageSourceSharing   = "sharing"
)

// resolveTargetSpec selects the target (gpu, cpu, memoryMB, zone) for instance
// creation. It collects all valid candidates from the "查询可用配比" step in the
// resolved availability zone, then narrows them using user-supplied Cpu/Memory.
//
// Decision rules:
//   - User gave both Cpu + Memory → must exactly match a candidate, else error.
//   - User gave only Cpu or only Memory → filter; 1 left = use it, >1 = ambiguity error.
//   - User gave neither → default to the first candidate (platform default).
//
// The returned zone is the availability zone the rest of the workflow (capacity /
// price / create / confirm) must use. It is the user-specified Zone when given,
// otherwise a zone from the live machine catalog for that GPU (preferring a
// sellable row). Missing zone data is an error: the workflow must not turn
// "unknown" into a fixed platform location.
func resolveTargetSpec(wfCtx *Context) (gpu, cpu, memoryMB float64, zone string, err error) {
	gpuType, _ := wfCtx.Params["GpuType"].(string)
	gpu = paramNum(wfCtx.Params, "Gpu", 1)

	result := wfCtx.Result("查询可用配比")
	if result == nil {
		return 0, 0, 0, "", fmt.Errorf("无法确定目标规格（CPU/Memory），「查询可用配比」步骤未返回结果")
	}

	zone = resolveTargetZone(result, gpuType, paramStr(wfCtx.Params, "Zone", ""))
	if zone == "" {
		if !catalogCarriesGPUType(result, gpuType) {
			return 0, 0, 0, "", fmt.Errorf("未找到 %s × %.0f 卡的可用配比。当前可部署的 GPU 机型：%s。请确认机型名称与卡数是否正确。",
				gpuType, gpu, availableTypeNames(result))
		}
		return 0, 0, 0, "", fmt.Errorf("未获取到 %s 的真实可用区，无法安全创建实例。请稍后重试或在可用区目录恢复后重新选择。", gpuType)
	}

	candidates := listSpecCandidates(result, gpuType, gpu, zone)
	if len(candidates) == 0 {
		// Grounded failure: list the GPU types the catalog actually returned so a
		// downstream reply can state real options instead of fabricating them.
		return 0, 0, 0, "", fmt.Errorf("未找到 %s × %.0f 卡的可用配比。当前可部署的 GPU 机型：%s。请确认机型名称与卡数是否正确。",
			gpuType, gpu, availableTypeNames(result))
	}

	_, hasCpu := wfCtx.Params["Cpu"]
	_, hasMem := wfCtx.Params["Memory"]

	// User gave neither — default to the first candidate.
	if !hasCpu && !hasMem {
		return gpu, candidates[0].CPU, candidates[0].MemoryMB, zone, nil
	}

	userCpu := paramNum(wfCtx.Params, "Cpu", 0)
	userMem := paramNum(wfCtx.Params, "Memory", 0)

	if hasCpu && hasMem {
		// Exact match required.
		for _, c := range candidates {
			if c.CPU == userCpu && c.MemoryMB == userMem {
				return gpu, c.CPU, c.MemoryMB, zone, nil
			}
		}
		return 0, 0, 0, "", fmt.Errorf("%s × %.0f 卡不支持 %.0fC/%.0fMB 的配比，合法选项：%s",
			gpuType, gpu, userCpu, userMem, formatCandidates(candidates))
	}

	// Filter by whichever single dimension the user specified.
	filtered := candidates
	if hasCpu {
		filtered = filterCandidates(filtered, func(c specCandidate) bool { return c.CPU == userCpu })
		if len(filtered) == 0 {
			return 0, 0, 0, "", fmt.Errorf("%s × %.0f 卡不支持 CPU=%.0f 的配比，合法选项：%s",
				gpuType, gpu, userCpu, formatCandidates(candidates))
		}
	}
	if hasMem {
		filtered = filterCandidates(filtered, func(c specCandidate) bool { return c.MemoryMB == userMem })
		if len(filtered) == 0 {
			return 0, 0, 0, "", fmt.Errorf("%s × %.0f 卡不支持 Memory=%.0fMB 的配比，合法选项：%s",
				gpuType, gpu, userMem, formatCandidates(candidates))
		}
	}

	if len(filtered) == 1 {
		return gpu, filtered[0].CPU, filtered[0].MemoryMB, zone, nil
	}

	// Multiple candidates remain after partial filter — ask user to narrow.
	return 0, 0, 0, "", fmt.Errorf("%s × %.0f 卡当前有多种合法配比：%s。请告诉我你想要哪一组 CPU/内存。",
		gpuType, gpu, formatCandidates(filtered))
}

// resolveTargetZone returns the availability zone to create in for the given GPU
// type. An explicit user Zone wins. Otherwise it scans the catalog for zones that
// carry this GPU, preferring the first "Normal" (sellable) row in the upstream
// catalog and otherwise preserving the catalog's first real row for the later
// capacity check. Returns "" when the catalog has no zone data; callers must
// fail rather than substitute a fixed default.
func resolveTargetZone(result map[string]any, gpuType, userZone string) string {
	if zone := strings.TrimSpace(userZone); zone != "" {
		return zone
	}
	var normalZones, allZones []string
	types, _ := result["AvailableInstanceTypes"].([]any)
	for _, t := range types {
		mt, _ := t.(map[string]any)
		if name, _ := mt["Name"].(string); name != gpuType {
			continue
		}
		z, _ := mt["Zone"].(string)
		if z == "" {
			continue
		}
		allZones = append(allZones, z)
		if status, _ := mt["Status"].(string); status == "" || status == "Normal" {
			normalZones = append(normalZones, z)
		}
	}
	if len(normalZones) > 0 {
		return normalZones[0]
	}
	if len(allZones) > 0 {
		return allZones[0]
	}
	return ""
}

func catalogCarriesGPUType(result map[string]any, gpuType string) bool {
	types, _ := result["AvailableInstanceTypes"].([]any)
	for _, raw := range types {
		entry, _ := raw.(map[string]any)
		name, _ := entry["Name"].(string)
		if strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(gpuType)) {
			return true
		}
	}
	return false
}

// availableTypeNames returns the distinct sellable GPU type names in the catalog
// result, joined for display (e.g. "4090、5090、V100S"). Used to ground a
// no-match failure reply so the user sees real options instead of a fabrication.
func availableTypeNames(result map[string]any) string {
	seen := map[string]bool{}
	var names []string
	types, _ := result["AvailableInstanceTypes"].([]any)
	for _, t := range types {
		mt, _ := t.(map[string]any)
		name, _ := mt["Name"].(string)
		if name == "" || seen[name] {
			continue
		}
		if status, _ := mt["Status"].(string); status != "" && status != "Normal" {
			continue // hide sold-out cards from the "what you can deploy" list
		}
		seen[name] = true
		names = append(names, name)
	}
	if len(names) == 0 {
		return "（暂无可用机型，请稍后再试）"
	}
	return strings.Join(names, "、")
}

func filterCandidates(cs []specCandidate, pred func(specCandidate) bool) []specCandidate {
	var out []specCandidate
	for _, c := range cs {
		if pred(c) {
			out = append(out, c)
		}
	}
	return out
}

// formatCandidates renders a human-readable list like "16C/64GB、16C/94GB".
func formatCandidates(cs []specCandidate) string {
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = fmt.Sprintf("%.0fC/%.0fGB", c.CPU, c.MemoryMB/1024)
	}
	return strings.Join(parts, "、")
}

// specCandidate represents one valid CPU/Memory combination for a GPU config.
type specCandidate struct {
	CPU      float64 // core count
	MemoryMB float64 // memory in MB
}

// listSpecCandidates enumerates all valid (CPU, MemoryMB) combinations from
// DescribeAvailableCompShareInstanceTypes for the given GPU type and count, in
// the target zone. Each Collection entry × each Memory value produces one
// candidate. Because the catalog query is no longer zone-filtered upstream (a
// GPU may appear in several zones), the zone filter here keeps candidates to the
// single resolved zone — avoiding cross-zone duplicates. Both the selected zone
// and each catalog row must be explicit; a missing Zone is not treated as a
// wildcard or a platform default.
func listSpecCandidates(result map[string]any, gpuType string, gpuCount float64, targetZone string) []specCandidate {
	targetZone = strings.TrimSpace(targetZone)
	if targetZone == "" {
		return nil
	}
	var candidates []specCandidate
	types, _ := result["AvailableInstanceTypes"].([]any)
	for _, t := range types {
		mt, _ := t.(map[string]any)
		name, _ := mt["Name"].(string)
		if name != gpuType {
			continue
		}
		entryZone, _ := mt["Zone"].(string)
		if strings.TrimSpace(entryZone) == "" || !strings.EqualFold(entryZone, targetZone) {
			continue
		}
		sizes, _ := mt["MachineSizes"].([]any)
		for _, s := range sizes {
			size, _ := s.(map[string]any)
			gpu, _ := size["Gpu"].(float64)
			if gpu != gpuCount {
				continue
			}
			collection, _ := size["Collection"].([]any)
			for _, c := range collection {
				col, _ := c.(map[string]any)
				cpu, _ := col["Cpu"].(float64)
				if cpu == 0 {
					continue
				}
				mems, _ := col["Memory"].([]any)
				for _, m := range mems {
					memGB, _ := m.(float64)
					if memGB > 0 {
						candidates = append(candidates, specCandidate{
							CPU:      cpu,
							MemoryMB: memGB * 1024,
						})
					}
				}
			}
		}
	}
	return candidates
}

// CreateInstanceDef returns the plain workflow definition for creating a
// CompShare GPU instance. Its exact step count is deliberately not part of the
// contract; the important boundary is resolve -> validate -> confirm -> execute.
// The create API is the final inventory authority; a second capacity preview
// after confirmation would add latency and could reject a valid create on a
// non-authoritative false negative.
//
// 形成执行草稿 sits between the queries and 检查库存 because everything after it
// must describe ONE resolution: stock is checked for the draft, price is quoted
// for the draft, the card shows the draft, and the create sends the copy of it
// the user sealed.
func CreateInstanceDef() *Definition {
	return &Definition{
		Name: "CreateInstanceWorkflow",
		// The model-facing text comes from the tool catalog. A second description
		// here would drift and expose server-owned steps as work for the Agent.
		Steps: []Step{
			stepQueryImages(false),
			stepQueryInstanceTypes(),
			// The plain flow reads the same inventory the guided flow does, so
			// createInventoryPoolSupport answers from live data on BOTH paths. Without
			// these the purchase-mode gate had no fact to read here and defaulted to
			// "supported", which is how a fully specified Spot create on a zone that
			// does not sell Spot reached the create API to be refused there.
			stepQueryOfficialGPUInventory(),
			stepQueryPodGPUInventory(),
			stepResolveGPUInventorySnapshot(),
			stepResolveCreateDraft(),
			stepCheckCapacity(),
			stepGetPrice(),
			stepResolveCreateConfirmation(),
			stepConfirmCreate(),
			stepCreateInstance(),
			stepDescribeInstance(),
		},
		ResultData:   createInstanceResultData,
		FailureDraft: createFailureDraft,
		// This workflow offers a guided multi-step selection form
		// (CreateInstanceGuidedDef) for an incomplete proposal; the catalog reads
		// this to expose IntakeGuided instead of the engine switching on the name.
		GuidedIntake: true,
		// The exact fields the guided form collects/corrects (GPU / zone / count /
		// CPU-memory / image source+selection / charge type).
		GuidedIntakeFields: []string{"GpuType", "Zone", "Gpu", "Cpu", "Memory", "ImageSource", "ImageName", "ChargeType"},
	}
}

// CreateInstanceGuidedDef returns the guided, Figma-style order flow for
// creating a CompShare GPU instance. The public action name stays
// CreateInstanceWorkflow to preserve the public tool and confirmation contract.
func CreateInstanceGuidedDef() *Definition {
	return &Definition{
		// The tool catalog owns the model-facing description; the steps below own
		// execution order.
		Name: "CreateInstanceWorkflow",
		Steps: []Step{
			stepQueryImages(true),
			// The legal machine catalog is not charge-type scoped. It may therefore be
			// fetched in the background before the user chooses billing; the capacity
			// calls that actually depend on billing remain below the charge-type card.
			stepQueryInstanceTypes(),
			// GPU inventory comes from TWO upstream implementations: the request's
			// zone_id selects the backend rather than filtering the result, so an
			// absent zone_id reaches only the official pools. Query both backends and
			// merge them against the live zone catalog.
			stepQueryOfficialGPUInventory(),
			stepQueryPodGPUInventory(),
			stepResolveGPUInventorySnapshot(),
			// The card order remains image-first.
			// Image must lead because the GPU list is constrained by the selected
			// image's SupportedGpuTypes, the per-zone capacity probe needs a concrete
			// image, and 卡数量 / CPU-内存 are gated by a capacity check that needs it
			// too. Image steps skip when the image is already known, so those flows
			// still start at GPU.
			stepGuidedChooseImageSource(),
			stepReQuerySelectedSourceImages(),
			// A named community search that matched nothing must fall back to browsing:
			// otherwise the facets/picker steps below inherit an empty catalog and the
			// flow dead-ends on a name the user never typed.
			stepBrowseCommunityWhenNameMatchedNothing(),
			// The platform's own category classification, fetched before the filter
			// card so it can offer 用途 rather than the raw tag strings of whichever
			// rows this page returned.
			stepQueryImageTagCatalog(),
			stepGuidedChooseImageFacets(),
			stepGuidedChooseImageTag(),
			stepGuidedChooseImageFamily(),
			stepGuidedChooseImage(),
			stepGuidedChooseChargeType(),
			// One capacity fan-out over every catalog (model, zone), shared by both
			// hardware cards. Image and charge type are settled here.
			stepProbeZoneCapacity(),
			stepGuidedChooseGPU(),
			stepGuidedChooseZone(),
			// Real creatability (ResourceEnough) for the resolved image+GPU+zone,
			// fetched before the count / CPU-memory steps so their options can be
			// gated by it rather than the static catalog + raw inventory.
			stepQueryCapacitySpecs(),
			stepGuidedChooseGPUCount(),
			stepGuidedChooseCPUMemory(),
			// Runs while 选择镜像's seal is still live — hence the rule that a
			// resolve step may not write Params, which would break that digest.
			stepResolveCreateDraft(),
			stepCheckCapacity(),
			stepGetPrice(),
			stepResolveCreateConfirmation(),
			stepConfirmCreateGuided(),
			stepCreateInstance(),
			stepDescribeInstance(),
		},
		ResultData:   createInstanceResultData,
		FailureDraft: createFailureDraft,
	}
}

func stepQueryImages(allowCommunityBrowse bool) Step {
	return Step{
		Name: "查询镜像",
		Type: StepToolCall,
		ToolFunc: func(wfCtx *Context) string {
			return imageCatalogToolForSource(paramStr(wfCtx.Params, "ImageSource", imageSourcePlatform))
		},
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			switch normalizedImageSource(paramStr(wfCtx.Params, "ImageSource", imageSourcePlatform)) {
			case imageSourceCommunity:
				if id := strings.TrimSpace(paramStr(wfCtx.Params, "CompShareImageId", "")); id != "" {
					return communityImageExactArgs(id), nil
				}
				name := paramStr(wfCtx.Params, "ImageName", "")
				if name == "" {
					if allowCommunityBrowse {
						return communityImageBrowseArgs(""), nil
					}
					return nil, fmt.Errorf("使用社区镜像创建实例时必须指定镜像名称（ImageName），请告诉我您想使用哪个社区镜像")
				}
				return map[string]any{"FuzzySearch": name}, nil
			case imageSourceCustom:
				// Keep custom-image reads tenant-scoped. The proposal-time snapshot
				// verifies a threaded exact ID by paging this same list, then
				// createImageResult merges that verified row when it is outside this
				// browse page. Passing the ID directly can change the upstream tenant
				// scope, so the workflow never uses a point-read here.
				return customImageBrowseArgs(), nil
			case imageSourceSharing:
				// Shared images use the same tenant-scoped list contract. The engine
				// verifies an exact id through the paginated list and createImageResult
				// merges a verified row that lies outside this browse page.
				return customImageBrowseArgs(), nil
			}
			args := map[string]any{
				"Limit": maxPlatformImageQueryLimit,
			}
			if id := paramStr(wfCtx.Params, "CompShareImageId", ""); id != "" {
				args["CompShareImageId"] = id
			}
			// Platform Name filtering is case-sensitive; the local catalog ranker
			// applies the supplied name without changing its intent.
			return args, nil
		},
	}
}

// imageCatalogToolForSource is the create flow's source-to-catalog contract.
// The result shape is handled centrally by formImageCatalog; this function owns
// only which live catalog the workflow reads.
func imageCatalogToolForSource(source string) string {
	switch normalizedImageSource(source) {
	case imageSourceCommunity:
		return "DescribeCommunityImages"
	case imageSourceCustom:
		return "DescribeCompShareCustomImages"
	case imageSourceSharing:
		return "DescribeCompShareSharingImages"
	default:
		return "DescribeCompShareImages"
	}
}

func communityImageExactArgs(id string) map[string]any {
	return map[string]any{
		"CompShareImageId": strings.TrimSpace(id),
		"Limit":            maxGuidedCommunityImageQueryLimit,
		"ExcludeReadme":    true,
	}
}

func communityImageBrowseArgs(name string) map[string]any {
	args := map[string]any{
		"Limit":         maxGuidedCommunityImageQueryLimit,
		"ExcludeReadme": true,
		"SortCondition": map[string]any{
			"Field": "CreatedCount",
			"ASC":   false,
		},
	}
	if name = strings.TrimSpace(name); name != "" {
		args["FuzzySearch"] = name
	}
	return args
}

// customImageBrowseArgs reads the current account's self-made images. The
// upstream list API caps a page at 100; exact IDs are intentionally verified by
// the engine's tenant-scoped paginated snapshot instead of being sent here.
func customImageBrowseArgs() map[string]any {
	return map[string]any{"Limit": maxCustomImageQueryLimit}
}

// stepReQuerySelectedSourceImages re-fetches the image catalog for the source the user
// chose in the guided source step, into the SAME "查询镜像" result the whole image
// selection reads. Any source switch replaces the initial catalog with the chosen
// source's, so the facets/picker/resolve/boot-disk steps never read a stale foreign-
// source catalog. Skipped when the source is unchanged from the initial (the first
// 查询镜像 already fetched it) or an explicit image is pinned.
func stepReQuerySelectedSourceImages() Step {
	return Step{
		Name:   "查询镜像",
		Type:   StepToolCall,
		SkipIf: shouldSkipSourceReQuery,
		ToolFunc: func(wfCtx *Context) string {
			return imageCatalogToolForSource(paramStr(wfCtx.Params, "ImageSource", imageSourcePlatform))
		},
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			switch normalizedImageSource(paramStr(wfCtx.Params, "ImageSource", imageSourcePlatform)) {
			case imageSourceCommunity:
				return communityImageBrowseArgs(paramStr(wfCtx.Params, "ImageName", "")), nil
			case imageSourceCustom:
				return customImageBrowseArgs(), nil
			case imageSourceSharing:
				return customImageBrowseArgs(), nil
			}
			args := map[string]any{"Limit": maxPlatformImageQueryLimit}
			if name := paramStr(wfCtx.Params, "ImageName", ""); name != "" {
				args["Name"] = name
			}
			return args, nil
		},
	}
}

// stepBrowseCommunityWhenNameMatchedNothing turns an empty fuzzy-name result
// into an unfiltered catalog browse. Non-empty narrowing remains intact.
func stepBrowseCommunityWhenNameMatchedNothing() Step {
	return Step{
		Name: "查询镜像",
		Type: StepToolCall,
		Tool: "DescribeCommunityImages",
		SkipIf: func(wfCtx *Context) (bool, error) {
			if normalizedImageSource(paramStr(wfCtx.Params, "ImageSource", "platform")) != "community" {
				return true, nil
			}
			if strings.TrimSpace(paramStr(wfCtx.Params, "ImageName", "")) == "" {
				return true, nil // already browsing the whole catalog
			}
			// Rescue ONLY an empty catalog — parsed exactly the way the picker parses
			// it, so this predicate cannot disagree with the card it protects.
			return formImageCatalog(wfCtx.Result("查询镜像"), "community").Len() > 0, nil
		},
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			return communityImageBrowseArgs(""), nil
		},
	}
}

func stepQueryInstanceTypes() Step {
	return Step{
		Name: "查询可用配比",
		Type: StepToolCall,
		Tool: "DescribeAvailableCompShareInstanceTypes",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			// Query the full catalog rather than locking to a single zone +
			// machine type. Reasons:
			//  (1) the upstream filters by Zone, so a hardcoded cn-wlcb-01 query
			//      silently drops cards that live only in another zone (e.g.
			//      2080Ti is cn-sh2-02-only) — resolveTargetZone then picks the
			//      right zone from the result;
			//  (2) on a no-match failure we can list the REAL available types
			//      instead of letting the narration round fabricate them.
			// Candidate + zone selection is done in-code (resolveTargetSpec).
			args := map[string]any{}
			if z := paramStr(wfCtx.Params, "Zone", ""); z != "" {
				args["Zone"] = z // honour an explicit zone (e.g. the deploy handler's ChosenZone)
				addZoneRegionAndID(wfCtx, args, z)
			}
			// Do not scope this catalog call to Spot: upstream returns an empty
			// machine-type list. Spot eligibility comes from GPU inventory.
			return args, nil
		},
	}
}

const (
	createOfficialGPUInventoryStep = "查询官方GPU库存"
	createPodGPUInventoryStep      = "查询Pod GPU库存"
	createGPUInventoryStep         = "查询GPU库存"
)

func stepQueryOfficialGPUInventory() Step {
	return Step{
		Name:     createOfficialGPUInventoryStep,
		Type:     StepToolCall,
		Tool:     "DescribeCompShareGpuInventory",
		Optional: true,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			args := map[string]any{}
			addWorkflowIdentityArgs(args, wfCtx.Runtime)
			return args, nil
		},
	}
}

func stepQueryPodGPUInventory() Step {
	return Step{
		Name:     createPodGPUInventoryStep,
		Type:     StepToolCall,
		Tool:     "DescribeCompShareGpuInventory",
		Optional: true,
		SkipIf: func(wfCtx *Context) (bool, error) {
			_, ok := deployment.PodSelectorZoneID(wfCtx.ZoneCatalog())
			return !ok, nil
		},
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			zoneID, ok := deployment.PodSelectorZoneID(wfCtx.ZoneCatalog())
			if !ok {
				return nil, fmt.Errorf("当前区域目录未提供 Pod 可用区")
			}
			args := map[string]any{"zone_id": zoneID}
			addWorkflowIdentityArgs(args, wfCtx.Runtime)
			return args, nil
		},
	}
}

// stepResolveGPUInventorySnapshot merges the two upstream implementations into
// one authoritative per-zone result. The upstream zone_id is a backend selector,
// not a result filter: an empty request reaches the official implementation,
// while any Pod zone id reaches the Pod implementation. The live zone catalog
// decides which backend owns each returned row, so an official zero can never
// shadow a real Pod count.
func stepResolveGPUInventorySnapshot() Step {
	return Step{
		Name: createGPUInventoryStep,
		Type: StepResolve,
		Resolve: func(wfCtx *Context) (map[string]any, error) {
			official := wfCtx.Result(createOfficialGPUInventoryStep)
			pod := wfCtx.Result(createPodGPUInventoryStep)
			_, podAttempted := deployment.PodSelectorZoneID(wfCtx.ZoneCatalog())
			snapshot := deployment.NewGPUInventorySnapshot(
				wfCtx.ZoneCatalog(),
				official, true, deployment.GPUInventoryPayloadAvailable(official),
				pod, podAttempted, deployment.GPUInventoryPayloadAvailable(pod),
			)
			return snapshot.ToResultMap(), nil
		},
	}
}

func stepCheckCapacity() Step {
	return Step{
		Name: "检查库存",
		Type: StepToolCall,
		Tool: "CheckCompShareResourceCapacity",
		// Capacity asks about the draft — the same resolution the card will show
		// and the create will send. It no longer calls resolveTargetSpec or
		// pickImageId: doing so made this a SECOND interpretation of the request,
		// which agreed with the draft's only because both are pure and nothing
		// moved between them. The draft's validations (placement, image
		// compatibility) already ran in the resolve step under the stricter
		// purchase=true form, so there is nothing left for this step to re-check.
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			draft, err := candidateCreateDraft(wfCtx)
			if err != nil {
				return nil, err
			}
			return draft.UpstreamCapacityArgs(), nil
		},
		CheckResult: func(wfCtx *Context, result map[string]any) CheckOutcome {
			draft, err := candidateCreateDraft(wfCtx)
			if err != nil {
				return CheckFailed(err.Error())
			}
			return checkExactCreateCapacity(draft, result)
		},
	}
}

func checkExactCreateCapacity(draft CreateExecutionDraft, result map[string]any) CheckOutcome {
	specs, _ := result["Specs"].([]any)
	if len(specs) == 0 {
		return CheckFailed("库存检查未返回任何规格信息，可能当前 GPU 型号不可用。")
	}
	gpu := draft.Args.GPU
	cpu := draft.Args.CPU
	memGB := draft.Args.Memory / 1024 // Specs.Mem is in GB; the draft's Memory is MB
	gt := draft.Args.GpuType
	if gt == "" {
		gt = "该 GPU"
	}
	for _, s := range specs {
		spec, _ := s.(map[string]any)
		sGpu, _ := spec["Gpu"].(float64)
		sCpu, _ := spec["Cpu"].(float64)
		sMem, _ := spec["Mem"].(float64)
		if sGpu != gpu || sCpu != cpu || sMem != memGB {
			continue
		}
		if enough, _ := spec["ResourceEnough"].(bool); enough {
			return CheckPassed()
		}
		return CheckFailedBecause(ReasonCapacitySoldOut,
			fmt.Sprintf("%s %.0f 卡 / %.0fC / %.0fGB 当前库存不足（售罄），请换一个规格或稍后再试。", gt, gpu, cpu, memGB))
	}
	return CheckFailed(fmt.Sprintf("库存中未找到 %s %.0f 卡 / %.0fC / %.0fGB 的规格组合，请确认配置是否正确。", gt, gpu, cpu, memGB))
}

const capacitySpecsStepName = "查询容量规格"

// stepQueryCapacitySpecs fetches CheckCompShareResourceCapacity.Specs for the
// resolved image + GPU + zone BEFORE the 卡数量 / CPU-内存 steps, so those option
// builders can gate combinations by real creatability (ResourceEnough) instead of
// the static legal catalog plus the unreliable raw GPU inventory. This is the same
// signal the official CLI uses for "in stock"; the authoritative negative still
// comes from the final 检查库存 re-check of the sealed config. Optional and skipped
// until a concrete image is resolved — capacity depends on the image and must not
// run before the user has one (asserted by TestCreateInstanceGuided_* timing tests).
func stepQueryCapacitySpecs() Step {
	return Step{
		Name:     capacitySpecsStepName,
		Type:     StepToolCall,
		Tool:     "CheckCompShareResourceCapacity",
		Optional: true,
		SkipIf: func(wfCtx *Context) (bool, error) {
			_, ok := guidedCapacityArgs(wfCtx)
			return !ok, nil
		},
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			args, ok := guidedCapacityArgs(wfCtx)
			if !ok {
				return nil, fmt.Errorf("容量规格查询缺少必要参数")
			}
			return args, nil
		},
	}
}

// guidedCapacityArgs builds the CheckCompShareResourceCapacity request from the
// current params (image + GPU + zone + boot disk + placement), mirroring the
// draft's UpstreamCapacityArgs but usable BEFORE the draft is formed. Returns
// ok=false when a concrete image or GPU is not yet resolved or the zone/placement
// cannot be determined — the caller then skips the fetch and the option builders
// fall back to the legal catalog (absence of a capacity signal is never "no stock").
func guidedCapacityArgs(wfCtx *Context) (map[string]any, bool) {
	_, _, _, zone, err := resolveTargetSpec(wfCtx)
	if err != nil || zone == "" {
		return nil, false
	}
	return guidedCapacityArgsForZone(wfCtx, zone)
}

// guidedCapacityArgsForZone is guidedCapacityArgs with the zone supplied rather
// than resolved from the current selection, so the same request can be asked
// about a zone the user has NOT chosen. That is the whole difference between
// reporting a sold-out zone after the fact and graying it out on the card:
// creatability is a property of (image, GPU, zone), and the zone card is built
// at the one moment where the first two are settled and the third is still open.
func guidedCapacityArgsForZone(wfCtx *Context, zone string) (map[string]any, bool) {
	return guidedCapacityArgsFor(wfCtx, paramStr(wfCtx.Params, "GpuType", ""), zone)
}

func guidedCapacityArgsFor(wfCtx *Context, gpuType, zone string) (map[string]any, bool) {
	if gpuType == "" || zone == "" {
		return nil, false
	}
	imageID := pickImageId(wfCtx.Params, createImageResult(wfCtx))
	if imageID == "" {
		return nil, false
	}
	placement, err := workflowZonePlacement(wfCtx, zone)
	if err != nil {
		return nil, false
	}
	disks, err := workflowCreateDisks(wfCtx, imageID, zone, gpuType, placement)
	if err != nil {
		return nil, false
	}
	args := deployment.BuildCapacityArgs(deployment.DeploymentDraft{
		Zone:               zone,
		GPUType:            gpuType,
		CompShareImageID:   imageID,
		ChargeType:         createChargeType(wfCtx.Params),
		Disks:              disks,
		MinimalCPUPlatform: workflowMinimalCPUPlatform(wfCtx, gpuType, zone),
	})
	return deployment.ApplyCapacityPlacementArgs(args, placement), true
}

const zoneCapacityStepName = "查询各可用区容量"

// stepProbeZoneCapacity asks once per catalog (model, zone) whether the resolved
// image can be created there. Both hardware cards share the result and disable
// known-unavailable choices before selection. The API requires a zone, so the
// fan-out cannot be represented by one request.
//
// This is what makes "every enabled option is creatable" true rather than
// approximate: a card count cannot answer it. The official CLI refuses
// `instance search --available` without `--image` for exactly this reason
// ("inventory depends on the image and disks"), and CheckCompShareResourceCapacity
// is the only call that accounts for image size, disk, spec and charge type.
//
// Optional, and skipped until a concrete image is resolved: absence of a
// capacity signal is never evidence of unavailability, so a probe that cannot
// run must leave the card exactly as it was rather than gray anything out.
func stepProbeZoneCapacity() Step {
	return Step{
		Name:     zoneCapacityStepName,
		Type:     StepToolCall,
		Tool:     "CheckCompShareResourceCapacity",
		Optional: true,
		SkipIf: func(wfCtx *Context) (bool, error) {
			skipGPU, err := shouldSkipGuidedGPUStep(wfCtx)
			if err != nil {
				return false, err
			}
			skipZone, err := shouldSkipGuidedZoneStep(wfCtx)
			if err != nil {
				return false, err
			}
			// Nothing to gray out if neither card is ever shown.
			if skipGPU && skipZone {
				return true, nil
			}
			return len(zoneCapacityProbeCalls(wfCtx)) == 0, nil
		},
		BuildArgsBatch: func(wfCtx *Context) ([]BatchCall, error) {
			return zoneCapacityProbeCalls(wfCtx), nil
		},
	}
}

// capacityComboKey names one (model, zone) probe. Both cards derive their key
// the same way, so a card can never look up a combination the probe filed under
// a different name.
func capacityComboKey(gpuType, zone string) string { return gpuType + "\x00" + zone }

// zoneCapacityProbeCalls builds one capacity request per (model, zone) the
// catalog offers. When the GPU is already pinned it narrows to that model.
//
// Returns nothing when the image is not yet resolved — the step then skips and
// both cards keep their ungated behavior.
func zoneCapacityProbeCalls(wfCtx *Context) []BatchCall {
	catalog := wfCtx.Result("查询可用配比")
	// Only models the resolved image can actually run: the GPU card disables the
	// rest on the image alone, so asking upstream about them buys nothing.
	models := filterModelsByImageSupport(
		guidedCandidateGPUModels(catalog),
		currentImageSupportedGPUs(wfCtx.Params, createImageResult(wfCtx)))
	if pinned := paramStr(wfCtx.Params, "GpuType", ""); pinned != "" {
		if skip, err := shouldSkipGuidedGPUStep(wfCtx); err == nil && skip {
			models = []string{pinned}
		}
	}
	var calls []BatchCall
	for _, gpuType := range models {
		for _, zone := range guidedExecutableCandidateZones(wfCtx, catalog, gpuType) {
			args, ok := guidedCapacityArgsFor(wfCtx, gpuType, zone)
			if !ok {
				continue
			}
			calls = append(calls, BatchCall{Key: capacityComboKey(gpuType, zone), Args: args})
		}
	}
	return calls
}

func filterModelsByImageSupport(models, supported []string) []string {
	if len(supported) == 0 {
		return models
	}
	var out []string
	for _, m := range models {
		if containsFold(supported, m) {
			out = append(out, m)
		}
	}
	return out
}

// guidedCandidateGPUModels lists the models the instance-type catalog offers, in
// catalog order and deduplicated. The GPU card further removes rows whose zone
// is absent from the turn's authoritative support-zone catalog; the capacity
// probe applies that same zone gate before it makes a request.
func guidedCandidateGPUModels(catalog map[string]any) []string {
	rows, _ := catalog["AvailableInstanceTypes"].([]any)
	var out []string
	seen := map[string]bool{}
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		name, _ := row["Name"].(string)
		if name == "" || seen[strings.ToLower(name)] {
			continue
		}
		seen[strings.ToLower(name)] = true
		out = append(out, name)
	}
	return out
}

// guidedCandidateZones lists the raw zones the instance-type catalog offers for
// this GPU, in catalog order and deduplicated. A type catalog can contain a
// region that the tenant's support-zone catalog does not permit, so callers that
// produce executable choices must use guidedExecutableCandidateZones below.
func guidedCandidateZones(catalog map[string]any, gpuType string) []string {
	if catalog == nil || gpuType == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	types, _ := catalog["AvailableInstanceTypes"].([]any)
	for _, t := range types {
		mt, _ := t.(map[string]any)
		if name, _ := mt["Name"].(string); name != gpuType {
			continue
		}
		if status, _ := mt["Status"].(string); status != "" && !strings.EqualFold(status, "Normal") {
			continue
		}
		zone, _ := mt["Zone"].(string)
		if zone == "" {
			continue
		}
		if seen[zone] {
			continue
		}
		seen[zone] = true
		out = append(out, zone)
	}
	return out
}

// guidedExecutableZone resolves one instance-type-catalog zone through the
// turn's support-zone snapshot. That snapshot is the single authority for both
// the placement IDs sent to upstream and the zones a form may offer. Returning
// the snapshot's canonical value also prevents casing in the two upstream lists
// from becoming a distinct form value.
func guidedExecutableZone(wfCtx *Context, zone string) (string, bool) {
	if wfCtx == nil {
		return "", false
	}
	entry, ok := wfCtx.ZoneCatalog().Entry(zone)
	if !ok || strings.TrimSpace(entry.Placement.Zone) == "" {
		return "", false
	}
	return entry.Placement.Zone, true
}

// guidedExecutableCandidateZones is the zone enumeration shared by the guided
// card and its capacity probe. It intersects the instance-type catalog with the
// authoritative support-zone catalog, so a card never offers a zone that the
// final create gate will reject before it reaches upstream.
func guidedExecutableCandidateZones(wfCtx *Context, catalog map[string]any, gpuType string) []string {
	seen := map[string]bool{}
	var out []string
	for _, raw := range guidedCandidateZones(catalog, gpuType) {
		zone, ok := guidedExecutableZone(wfCtx, raw)
		if !ok || seen[strings.ToLower(zone)] {
			continue
		}
		seen[strings.ToLower(zone)] = true
		out = append(out, zone)
	}
	return out
}

// zoneCreatability reports, per zone, what the probe established. A zone is
// present ONLY when a call for it succeeded AND returned a usable Specs[]: a
// failed call, a call the batch bound never made, and an empty spec list are all
// "we do not know", and the caller must leave those zones alone.
func zoneCreatability(result map[string]any) map[string]bool {
	return comboCreatability(result)
}

// comboCreatability maps capacityComboKey(model, zone) -> creatable. An entry is
// only present when that probe actually answered: a failed call or a response
// with no capacity signal stays ABSENT, which both cards read as unknown rather
// than as a refusal.
func comboCreatability(result map[string]any) map[string]bool {
	outcomes := BatchResults(result)
	if len(outcomes) == 0 {
		return nil
	}
	known := map[string]bool{}
	for _, o := range outcomes {
		if !o.OK || o.Key == "" {
			continue
		}
		specs := parseCapacitySpecs(o.Result)
		if !capacityHasSignal(specs) {
			continue
		}
		known[o.Key] = capacityCreatable(specs)
	}
	if len(known) == 0 {
		return nil
	}
	return known
}

// zoneCreatabilityFor narrows the combo map to one model, keyed by zone, which
// is the shape the zone card has always consumed. It looks each candidate up by
// the same key the probe filed it under rather than scanning — the caller
// already knows which zones it is going to render.
func zoneCreatabilityFor(combos map[string]bool, gpuType string, zones []string) map[string]bool {
	if len(combos) == 0 || gpuType == "" {
		return nil
	}
	out := map[string]bool{}
	for _, zone := range zones {
		if ok, answered := combos[capacityComboKey(gpuType, zone)]; answered {
			out[zone] = ok
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// gpuModelCreatable answers the GPU card's question: is this model creatable in
// AT LEAST ONE of the zones it is offered in? Unknown combinations count as
// possible — a probe that could not answer must not gray anything out.
func gpuModelCreatable(combos map[string]bool, gpuType string, zones []string) (creatable, known bool) {
	for _, zone := range zones {
		ok, answered := combos[capacityComboKey(gpuType, zone)]
		if !answered {
			return true, false
		}
		known = true
		if ok {
			return true, true
		}
	}
	return false, known
}

func stepGetPrice() Step {
	return Step{
		Name: "查询价格",
		Type: StepToolCall,
		Tool: "GetCompShareInstanceUserPrice",
		// Price quotes the draft, so the number on the confirm card describes the
		// instance that will actually be created. Like capacity, it selects fields
		// from the one resolution instead of performing its own.
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			draft, err := candidateCreateDraft(wfCtx)
			if err != nil {
				return nil, err
			}
			return draft.UpstreamPriceArgs(), nil
		},
	}
}

func stepGuidedChooseGPU() Step {
	return Step{
		Name:              "选择 GPU",
		Type:              StepConfirm,
		SkipIf:            shouldSkipGuidedGPUStep,
		BuildForm:         buildGuidedGPUForm,
		ApplyOverrides:    applyGuidedGPUOverrides,
		ConfirmSubmitMode: ConfirmSubmitContinue,
		PromoteOnConfirm: func(wfCtx *Context) error {
			gpuType, err := ensureGuidedGPUType(wfCtx)
			if err != nil {
				return err
			}
			return applyGuidedGPUOverrides(wfCtx, map[string]string{"GpuType": gpuType})
		},
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			gpuType, err := ensureGuidedGPUType(wfCtx)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"workflow": "CreateInstanceWorkflow",
				"step":     guidedStepLabel(wfCtx, guidedStepGPU),
				"GpuType":  gpuType,
			}, nil
		},
	}
}

func stepGuidedChooseZone() Step {
	return Step{
		Name:              "选择可用区",
		Type:              StepConfirm,
		SkipIf:            shouldSkipGuidedZoneStep,
		BuildForm:         buildGuidedZoneForm,
		ApplyOverrides:    applyGuidedZoneOverrides,
		ConfirmSubmitMode: ConfirmSubmitContinue,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			gpuType, err := ensureGuidedGPUType(wfCtx)
			if err != nil {
				return nil, err
			}
			zone, err := ensureGuidedZone(wfCtx)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"workflow": "CreateInstanceWorkflow",
				"step":     guidedStepLabel(wfCtx, guidedStepZone),
				"GpuType":  gpuType,
				"Zone":     zone,
			}, nil
		},
	}
}

func stepGuidedChooseGPUCount() Step {
	return Step{
		Name:              "选择卡数量",
		Type:              StepConfirm,
		SkipIf:            shouldSkipGuidedGPUCountStep,
		BuildForm:         buildGuidedGPUCountForm,
		ApplyOverrides:    applyGuidedGPUCountOverrides,
		ConfirmSubmitMode: ConfirmSubmitContinue,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
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
			return map[string]any{
				"workflow": "CreateInstanceWorkflow",
				"step":     guidedStepLabel(wfCtx, guidedStepGPUCount),
				"GpuType":  gpuType,
				"Zone":     zone,
				"Gpu":      gpu,
			}, nil
		},
	}
}

func stepGuidedChooseCPUMemory() Step {
	return Step{
		Name:              "选择 CPU/内存",
		Type:              StepConfirm,
		SkipIf:            shouldSkipGuidedCPUMemoryStep,
		BuildForm:         buildGuidedCpuMemoryForm,
		ApplyOverrides:    applyGuidedCpuMemoryOverrides,
		ConfirmSubmitMode: ConfirmSubmitContinue,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
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
			current, opts := guidedCpuMemoryFormOptions(wfCtx, wfCtx.Result("查询可用配比"), gpuType, zone, gpu, wfCtx.Params, wfCtx.Result("查询GPU库存"))
			if current == "" || len(opts) == 0 {
				return nil, fmt.Errorf("%s 在 %s 的 %.0f 卡暂无可选 CPU/内存规格，请换一个可用区或卡数量", gpuType, zone, gpu)
			}
			return map[string]any{
				"workflow":  "CreateInstanceWorkflow",
				"step":      guidedStepLabel(wfCtx, guidedStepCPUMemory),
				"GpuType":   gpuType,
				"Zone":      zone,
				"Gpu":       gpu,
				"CpuMemory": current,
			}, nil
		},
	}
}

// stepGuidedChooseImageSource is the FIRST of the two-stage image flow: it picks the
// image SOURCE alone (platform/community). It comes before the source re-query and the
// facets step so that a source change re-queries that source's real catalog and the
// facets/picker are built from it — never from the previous source's stale listing.
func stepGuidedChooseImageSource() Step {
	return Step{
		Name:              "选择镜像来源",
		Type:              StepConfirm,
		SkipIf:            shouldSkipGuidedImageSourceStep,
		BuildForm:         buildGuidedImageSourceForm,
		ApplyOverrides:    applyGuidedImageSourceOverrides,
		ConfirmSubmitMode: ConfirmSubmitContinue,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			return map[string]any{
				"workflow":    "CreateInstanceWorkflow",
				"step":        guidedStepLabel(wfCtx, guidedStepImageSource),
				"ImageSource": paramStr(wfCtx.Params, "ImageSource", "platform"),
			}, nil
		},
	}
}

const imageTaxonomyStepName = "查询镜像分类"

// stepQueryImageTagCatalog fetches the public catalog's own classification so the
// filter card can offer 用途 categories instead of the raw tag strings that happen
// to appear on this page of the catalog. A custom source is a private account
// inventory, not a public semantic catalog, so it goes straight to its real image
// list without this taxonomy overlay.
//
// Optional and parameterless. A missing classification must leave the card exactly
// as it was — degrade to the flat tag facet — never gray out or hide an image,
// because "we could not fetch the categories" says nothing about any image.
//
// Skipped once an image is already pinned: there is no browsing left to filter.
func stepQueryImageTagCatalog() Step {
	return Step{
		Name:     imageTaxonomyStepName,
		Type:     StepToolCall,
		Tool:     "DescribeCompShareImageTags",
		Optional: true,
		SkipIf: func(wfCtx *Context) (bool, error) {
			return hasExplicitImageSelection(wfCtx.Params) || tenantImageInventorySelected(wfCtx), nil
		},
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			args := map[string]any{}
			addWorkflowIdentityArgs(args, wfCtx.Runtime)
			return args, nil
		},
	}
}

// createImageTaxonomy is the workflow's single view of the platform classification.
func createImageTaxonomy(wfCtx *Context) *deployment.ImageTaxonomy {
	return deployment.ParseImageTaxonomy(wfCtx.Result(imageTaxonomyStepName))
}

// imageCandidateSet is the one candidate set the whole image flow reads. Every
// number a card states and every option it offers is a projection of it, so the
// card cannot promise a population the next card does not have.
type imageCandidateSet struct {
	snap *deployment.ImageCatalogSnapshot
	// base survives the hard gates and the structured request; no facet applied.
	// The TYPE facet counts over this.
	base []deployment.ImageSelection
	// afterType is base narrowed by the chosen ImageType. The TAG facet counts over
	// this — that is the whole reason the tag question is a later card.
	afterType []deployment.ImageSelection
	// final is what the picker offers and what "共 N 个" counts.
	final []deployment.ImageSelection
}

// buildImageCandidateSet takes zoneIsPod as an EXPLICIT argument rather than
// reading the ZoneIsPod param, which was the bug. ZoneIsPod is a denormalized cache
// that syncGuidedZoneMeta only writes at the zone card — and under the image-first
// order the picker runs BEFORE that card, so a zone pinned in the request reached
// here with the param absent (read as non-pod). The pod/container filter never
// applied, the picker offered and defaulted to a VM-only image, and the create gate
// refused it at the very end ("... 不是容器镜像，不能用于 上海二A"). The caller now
// resolves the flag from the zone catalog (createZoneIsPod), the same authority the
// create gate uses, so it cannot be stale or unset.
func buildImageCandidateSet(params map[string]any, images map[string]any, gpuType string, taxonomy *deployment.ImageTaxonomy, zoneIsPod bool) imageCandidateSet {
	return buildImageCandidateSetForRequest(params, images, taxonomy, deployment.ImageRequest{
		Name:         paramStr(params, "ImageName", ""),
		RequestedGPU: gpuType,
		Zone:         deployment.ZoneConstraint{Zone: paramStr(params, "Zone", ""), IsPod: zoneIsPod},
	})
}

// buildImageCandidateSetForRequest ranks the supplied structured request against
// the live catalog and applies the selected form facets.
func buildImageCandidateSetForRequest(params map[string]any, images map[string]any, taxonomy *deployment.ImageTaxonomy, request deployment.ImageRequest) imageCandidateSet {
	snap := formImageCatalog(images, paramStr(params, "ImageSource", "platform"))
	base := deployment.RankImages(snap, request)
	wantType := strings.TrimSpace(paramStr(params, "ImageType", ""))
	wantTag := strings.TrimSpace(paramStr(params, "ImageTag", ""))
	wantCategory := strings.TrimSpace(paramStr(params, "ImageCategory", ""))
	wantFamily := strings.TrimSpace(paramStr(params, "ImageFamily", ""))

	afterType := base
	if wantType != "" {
		afterType = filterSelections(base, func(sel deployment.ImageSelection) bool {
			return imageSelectionMatchesFacets(snap, sel.ID, wantType, "")
		})
	}
	final := afterType
	if wantTag != "" || wantCategory != "" || wantFamily != "" {
		final = filterSelections(afterType, func(sel deployment.ImageSelection) bool {
			return imageSelectionMatchesFacets(snap, sel.ID, "", wantTag) &&
				imageSelectionMatchesCategory(snap, taxonomy, sel.ID, wantCategory) &&
				imageSelectionMatchesFamily(snap, sel.ID, wantFamily)
		})
	}
	return imageCandidateSet{snap: snap, base: base, afterType: afterType, final: final}
}

func filterSelections(in []deployment.ImageSelection, keep func(deployment.ImageSelection) bool) []deployment.ImageSelection {
	out := make([]deployment.ImageSelection, 0, len(in))
	for _, sel := range in {
		if keep(sel) {
			out = append(out, sel)
		}
	}
	return out
}

// createImageCandidates builds the candidate set from the workflow context, so the
// facet cards, the tag card and the picker all read the same parameters — and the
// same authoritative pod flag resolved from the zone catalog.
func createImageCandidates(wfCtx *Context) imageCandidateSet {
	images := createImageResult(wfCtx)
	request := deployment.ImageRequest{
		Name:         paramStr(wfCtx.Params, "ImageName", ""),
		RequestedGPU: paramStr(wfCtx.Params, "GpuType", ""),
		Zone: deployment.ZoneConstraint{
			Zone:  paramStr(wfCtx.Params, "Zone", ""),
			IsPod: createZoneIsPod(wfCtx),
		},
	}
	return buildImageCandidateSetForRequest(
		wfCtx.Params, images, createImageTaxonomy(wfCtx), request,
	)
}

// createImageFamilies projects the current, already-filtered candidate set into
// the source-independent family hierarchy used by the guided picker. Community
// groups stay grouped; flat catalog rows are intentional one-version families.
func createImageFamilies(wfCtx *Context) []deployment.ImageFamily {
	set := createImageCandidates(wfCtx)
	return deployment.GroupImageFamilies(candidateEntries(set.snap, set.final))
}

// createZoneIsPod resolves the pinned zone's pod flag from the zone catalog — the
// same authority validateSelectedImageCompatibility (the create gate) reads. It
// exists because the ZoneIsPod param is written lazily at the zone card, which runs
// after the image picker: trusting it there let a request-pinned pod zone read as
// non-pod. Falls back to the cached param only when the catalog cannot resolve the
// zone (a zone it does not carry), which is the best available answer then.
func createZoneIsPod(wfCtx *Context) bool {
	if zone := strings.TrimSpace(paramStr(wfCtx.Params, "Zone", "")); zone != "" {
		if entry, err := workflowZoneEntry(wfCtx, zone); err == nil {
			return entry.Placement.IsPod
		}
	}
	return paramBool(wfCtx.Params, "ZoneIsPod", false) || paramBool(wfCtx.Params, "IsPodZone", false)
}

func stepGuidedChooseImageFacets() Step {
	return Step{
		Name:              "选择镜像筛选",
		Type:              StepConfirm,
		SkipIf:            shouldSkipGuidedImageFacetsStep,
		BuildForm:         buildGuidedImageFacetsForm,
		ApplyOverrides:    applyGuidedImageFacetsOverrides,
		ConfirmSubmitMode: ConfirmSubmitContinue,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			return map[string]any{
				"workflow":      "CreateInstanceWorkflow",
				"step":          guidedStepLabel(wfCtx, guidedStepImageFacets),
				"ImageType":     paramStr(wfCtx.Params, "ImageType", ""),
				"ImageCategory": paramStr(wfCtx.Params, "ImageCategory", ""),
			}, nil
		},
	}
}

// stepGuidedChooseImageTag asks the raw-tag question after the type question, so
// the tags offered are the ones the chosen type actually leaves behind.
func stepGuidedChooseImageTag() Step {
	return Step{
		Name:              "选择镜像标签",
		Type:              StepConfirm,
		SkipIf:            shouldSkipGuidedImageTagStep,
		BuildForm:         buildGuidedImageTagForm,
		ApplyOverrides:    applyGuidedImageTagOverrides,
		ConfirmSubmitMode: ConfirmSubmitContinue,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			return map[string]any{
				"workflow": "CreateInstanceWorkflow",
				"step":     guidedStepLabel(wfCtx, guidedStepImageTag),
				"ImageTag": paramStr(wfCtx.Params, "ImageTag", ""),
			}, nil
		},
	}
}

// stepGuidedChooseImageFamily selects the user-recognisable image series before
// resolving a concrete version. It is data-driven: any source whose candidates are
// all singleton families skips it and retains the existing one-card image picker.
func stepGuidedChooseImageFamily() Step {
	return Step{
		Name:              "选择镜像系列",
		Type:              StepConfirm,
		SkipIf:            shouldSkipGuidedImageFamilyStep,
		BuildForm:         buildGuidedImageFamilyForm,
		ApplyOverrides:    applyGuidedImageFamilyOverrides,
		ConfirmSubmitMode: ConfirmSubmitContinue,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			current, opts, _ := guidedImageFamilyFormOptionsForContext(wfCtx)
			if len(opts) == 0 {
				return nil, fmt.Errorf("未找到可选镜像系列，请换一个镜像来源或稍后再试")
			}
			if current == "" {
				current = opts[0].Value
			}
			return map[string]any{
				"workflow":    "CreateInstanceWorkflow",
				"step":        guidedStepLabel(wfCtx, guidedStepImageFamily),
				"ImageFamily": current,
			}, nil
		},
	}
}

func stepGuidedChooseImage() Step {
	return Step{
		Name:              "选择镜像",
		Type:              StepConfirm,
		SkipIf:            shouldSkipGuidedImageStep,
		BuildForm:         buildGuidedImageForm,
		ApplyOverrides:    applyGuidedImageOverrides,
		ConfirmSubmitMode: ConfirmSubmitContinue,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			gpuType := paramStr(wfCtx.Params, "GpuType", "")
			current, opts, _ := guidedImageFormOptionsForContext(wfCtx, gpuType)
			if len(opts) == 0 {
				return nil, fmt.Errorf("未找到可选镜像，请换一个镜像来源或稍后再试")
			}
			if current == "" {
				current = opts[0].Value
			}
			return map[string]any{
				"workflow": "CreateInstanceWorkflow",
				"step":     guidedStepLabel(wfCtx, guidedStepImage),
				"ImageId":  current,
				"GpuType":  gpuType,
			}, nil
		},
	}
}

// createChargeType normalizes the create path to the current upstream billing
// contract: pay-as-you-go/hourly uses Postpay. Dynamic is a deprecated input
// spelling kept only for backward compatibility with older LLM/tool args.
func createChargeType(params map[string]any) string {
	return deployment.NormalizeChargeType(paramStr(params, "ChargeType", ""))
}

func imageMapByID(images map[string]any, id string) map[string]any {
	if images == nil || id == "" {
		return nil
	}
	if groups, ok := images["CompshareImageGroup"].([]any); ok {
		for _, g := range groups {
			gm, _ := g.(map[string]any)
			if gm == nil {
				continue
			}
			data, _ := gm["Data"].([]any)
			for _, d := range data {
				dm, _ := d.(map[string]any)
				if got, _ := dm["CompShareImageId"].(string); got == id {
					return dm
				}
			}
		}
		return nil
	}
	imageSet, _ := images["ImageSet"].([]any)
	for _, item := range imageSet {
		img, _ := item.(map[string]any)
		if img == nil {
			continue
		}
		if got, _ := img["CompShareImageId"].(string); got == id {
			return img
		}
	}
	return nil
}

func stepConfirmCreate() Step {
	return Step{
		Name: "确认创建",
		Type: StepConfirm,
		// Editable selection form (v1, select-only). Consumed only when the
		// HTTP wires ConfirmEditsFunc for opted-in clients; other clients and
		// the deploy_model saga ignore these three fields.
		BuildForm:      buildCreateConfirmForm,
		ApplyOverrides: applyCreateOverrides,
		// An edit re-runs from the draft, not from stock: the edited params must
		// be re-resolved before anything is asked about them, or capacity and
		// price would answer about the previous combination. Naming the boundary
		// rather than the steps also means this cannot fall out of step with the
		// definition's order — the engine walks the definition.
		RevalidateFrom:   createDraftStepName,
		PromoteOnConfirm: promoteCreateDraft,
		BuildArgs:        buildCreateConfirmArgs,
	}
}

func stepConfirmCreateGuided() Step {
	return Step{
		Name:             "确认创建",
		Type:             StepConfirm,
		BuildForm:        buildGuidedFinalForm,
		ApplyOverrides:   applyCreateOverrides,
		RevalidateFrom:   createDraftStepName,
		PromoteOnConfirm: promoteCreateDraft,
		BuildArgs:        buildCreateConfirmArgs,
	}
}

// createDraftStepName is the resolve step that forms the create draft. It is the
// single place every derived create value is decided, and the boundary the
// confirm gate re-runs from after a form edit.
const createDraftStepName = "形成执行草稿"

// createDraftKey is where the CONFIRMED execution draft lives inside
// Context.Params. Only PromoteOnConfirm writes it, and only once the user has
// approved the card built from the candidate.
//
// The candidate draft lives in StepResults[createDraftStepName] instead, and the
// difference is the whole design. Params is what seal() hashes, so a draft there
// is inside the sealed contract — which is exactly why the resolve step may not
// put it there: the guided create runs six selection gates before the draft is
// formed, Run seals after each one, and writing Params under a live seal would
// break the digest of a card the user legitimately confirmed. "Computed" and
// "agreed to" are different facts and now live in different places.
//
// The draft is also deliberately separate from the user's request params
// (GpuType / Cpu / Memory / Zone). Those record what the user ASKED for and must
// keep their exact shape across confirm-form edits: to resolveTargetSpec an
// ABSENT Cpu means "platform default" while a PRESENT one means "must match
// exactly", so writing a resolved CPU back over Params["Cpu"] would silently
// change the meaning of the next re-resolve. The draft records what will
// actually be sent.
// What lives under it is the ENCODED draft (CreateExecutionDraft.ToContractMap),
// never the struct — see CreateExecutionDraft for why storing the struct would
// silently dissolve the seal. Its internal key names belong to the codec in
// create_draft.go and are not read anywhere else.
const createDraftKey = "__create_draft"

// materializeCreateDraft resolves every derived create parameter once — zone,
// CPU/memory, card count, image id, charge type, minimal CPU platform, system
// disks and placement — and returns the draft. It stores nothing; see the note at the
// end of this comment.
//
// It runs as the createDraftStepName resolve step, BEFORE capacity and price, so
// those two consume the same resolution the card shows and the create sends
// instead of interpreting the request a second and third time. On a confirm-form
// edit the gate re-runs from that step (Step.RevalidateFrom), so the draft is
// rebuilt from the edited params before stock and price are re-checked, and the
// version finally promoted and sealed is the one the user approved.
//
// It returns the draft rather than storing it: a resolve step's product is a
// candidate, and runResolveStep rejects any Resolve that writes Params. See
// createDraftKey.
func materializeCreateDraft(wfCtx *Context) (map[string]any, error) {
	gpu, cpu, mem, zone, err := resolveTargetSpec(wfCtx)
	if err != nil {
		return nil, err
	}
	image := selectCreateImage(wfCtx)
	imageId := image.ID
	if paramStr(wfCtx.Params, "ImageSource", "platform") == "community" && imageId == "" {
		return nil, fmt.Errorf("社区镜像未返回有效的镜像 ID，无法创建实例（请确认社区镜像名称是否正确）")
	}
	if imageId == "" {
		return nil, createImageUnavailableError(wfCtx.Params)
	}
	gt, _ := wfCtx.Params["GpuType"].(string)
	instanceName := paramStr(wfCtx.Params, "Name", "")
	if instanceName != "" {
		instanceName, err = validatedCompShareResourceName(instanceName, "实例名称", 63)
		if err != nil {
			return nil, err
		}
	}
	placement, err := workflowZonePlacement(wfCtx, zone)
	if err != nil {
		return nil, err
	}
	// Both validations run HERE, once, before capacity, price or the card. The
	// purchase=true form is the strictest (it alone requires AzGroup on a pod
	// zone), so passing it subsumes the capacity step's weaker purchase=false
	// check — which is why capacity no longer runs one of its own.
	if err := validateCreatePlacement(wfCtx, placement, true); err != nil {
		return nil, err
	}
	if err := validateSelectedImageCompatibility(wfCtx, imageId, placement); err != nil {
		return nil, err
	}
	disks, err := workflowCreateDisks(wfCtx, imageId, zone, gt, placement)
	if err != nil {
		return nil, err
	}

	// The typed decision. The selection is carried WHOLE, not re-derived for
	// display: the card renders Image.Name, the create sends Args.CompShareImageID,
	// and both come from the one selectCreateImage call above.
	draft := CreateExecutionDraft{
		Args: CreateInstanceArgs{
			Zone:               zone,
			GpuType:            gt,
			GPU:                gpu,
			CPU:                cpu,
			Memory:             mem,
			CompShareImageID:   imageId,
			ChargeType:         createChargeType(wfCtx.Params),
			MachineType:        deployment.MachineTypeGPU,
			MinimalCPUPlatform: workflowMinimalCPUPlatform(wfCtx, gt, zone),
			LoginMode:          deployment.LoginModeConsole,
			Disks:              disks,
			Name:               instanceName,
		},
		Image:     image,
		Placement: placement,
	}
	// Encoded on the way out: what Params, StepResults and the seal store is the
	// plain map form, never the struct. See CreateExecutionDraft.
	return draft.ToContractMap(), nil
}

// stepResolveCreateDraft forms the create draft. It is a StepResolve, so it calls
// no tool and no model and may not write Params — see createDraftKey for why
// that matters on the guided path.
func stepResolveCreateDraft() Step {
	return Step{
		Name:    createDraftStepName,
		Type:    StepResolve,
		Resolve: materializeCreateDraft,
	}
}

// createConfirmationStepName joins the resolved execution with the price quoted
// for it. It is a second resolve step rather than part of the draft because the
// draft must exist BEFORE the price: stock and price are both asked about the
// draft, so the quote only exists once the draft has already been formed.
const createConfirmationStepName = "形成确认快照"

// stepResolveCreateConfirmation builds what the user will actually be shown and
// what the seal will actually freeze.
func stepResolveCreateConfirmation() Step {
	return Step{
		Name:    createConfirmationStepName,
		Type:    StepResolve,
		Resolve: materializeCreateConfirmation,
	}
}

// materializeCreateConfirmation joins the draft with the estimate quoted for it.
//
// The price text is rendered HERE, once. The card reads it and PromoteOnConfirm
// seals it, so the sentence the user read is the sentence the contract records.
// Rendering at card time and rebuilding at promote time would be two computations
// agreeing by luck — which is precisely the shape this convergence has spent eight
// commits removing, and it would be worse here than elsewhere: the thing that
// diverged would be the price the user believed they were agreeing to.
//
// It records no observation time. A resolve step must be replayable from a trace,
// and time.Now() here would make it a different computation on every replay; if a
// quote timestamp is ever wanted it has to be captured when the price TOOL
// returns, not when a pure step reads its result.
func materializeCreateConfirmation(wfCtx *Context) (map[string]any, error) {
	draft, err := candidateCreateDraft(wfCtx)
	if err != nil {
		return nil, err
	}
	return CreateConfirmationSnapshot{
		Execution: draft,
		// nil when upstream quoted nothing usable — an absent price is shown as
		// absent, never as zero.
		EstimatedPrice: extractEstimatedPrice(wfCtx.Result("查询价格"), draft.Args.ChargeType),
	}.ToContractMap(), nil
}

// candidateCreateConfirmation returns the typed snapshot the confirmation step
// produced. Like the draft, its absence is a hard error rather than a rebuild.
func candidateCreateConfirmation(wfCtx *Context) (CreateConfirmationSnapshot, error) {
	stored := wfCtx.Result(createConfirmationStepName)
	if len(stored) == 0 {
		return CreateConfirmationSnapshot{}, fmt.Errorf("尚未形成确认快照，无法继续创建")
	}
	return ParseCreateConfirmationSnapshot(stored)
}

// candidateCreateDraft returns the typed draft the resolve step produced: what
// WOULD be created, for capacity, price and the confirm card to consume.
//
// Its absence is a hard error, never a rebuild. Re-deriving here would restore
// exactly what this step exists to remove — a second interpretation of the
// request, agreeing with the first only for as long as nothing between them
// changes.
func candidateCreateDraft(wfCtx *Context) (CreateExecutionDraft, error) {
	stored := wfCtx.Result(createDraftStepName)
	if len(stored) == 0 {
		return CreateExecutionDraft{}, fmt.Errorf("尚未形成执行草稿，无法继续创建")
	}
	return ParseCreateExecutionDraft(stored)
}

// createFailureDraft reports the resolved execution the failed step was working
// from, for the workflow's failure record. It is the create's Definition.
// FailureDraft, and like ResultData it hands the engine an encoding the engine
// never looks inside.
//
// It returns the CANDIDATE — 形成执行草稿's own result — and not the sealed copy,
// on purpose. The failure this record has to answer for is the capacity gate's
// 库存不足, which is reached before any create is authorised: on the plain path
// nothing is sealed at all, and on the guided path what IS sealed authorised an
// image choice. The candidate is the only thing that describes what the failed
// step was actually asking about. Whether it was ever approved is a separate
// question, and StepFailure.Sealed is where the record answers it rather than
// leaving a reader to infer it from a contract's presence.
//
// Nil before 形成执行草稿 has run — an early failure resolved no candidate, and
// saying so is better than returning a half-built one.
func createFailureDraft(wfCtx *Context) map[string]any {
	return wfCtx.Result(createDraftStepName)
}

// promoteCreateDraft copies the approved candidate into Params so seal() covers
// it. It is the create's Step.PromoteOnConfirm and the only writer of
// createDraftKey: reaching Params is what turns a computed candidate into the
// confirmed contract, so it happens exactly when the user says yes.
//
// Params must not alias StepResults, or a later write to either would diverge the
// live params from the sealed digest and fail-stop a create the user correctly
// approved.
func promoteCreateDraft(wfCtx *Context) error {
	snapshot, err := candidateCreateConfirmation(wfCtx)
	if err != nil {
		return err
	}
	// The same snapshot the card rendered, not a rebuilt draft. Both the
	// decode above and the encode below copy the disks, so what lands in Params is
	// independent of StepResults all the way down.
	wfCtx.Params[createDraftKey] = snapshot.ToContractMap()
	return nil
}

func buildCreateConfirmArgs(wfCtx *Context) (map[string]any, error) {
	snapshot, err := candidateCreateConfirmation(wfCtx)
	if err != nil {
		return nil, err
	}
	draft := snapshot.Execution
	zone := draft.Args.Zone
	// The card is a projection of the draft. Every executable value is read FROM
	// it — never re-derived — so what is shown is what is sealed and executed. The
	// image NAME comes from the draft's carried selection, not a second lookup:
	// that is what stops the card naming one image while the create sends another.
	//
	// The price TEXT is part of that contract too, which is why its charge type is
	// read off the draft rather than re-normalised from Params. The two agree on
	// every path today. No price means no confirmation card: a paid operation must
	// show a usable quote before approval.
	//
	// Reaching here without a price is narrow: 查询价格 is not Optional, so a
	// transport error or a non-zero RetCode has already fail-stopped the workflow
	// upstream of this gate. What is left is a RetCode-0 response quoting nothing
	// usable for the resolved charge type — which no capture in this repo shows,
	// and which the live response makes unlikely, since one call returns a row for
	// every charge type at once (Postpay/Dynamic/Day/Month/Spot, confirmed against
	// a live capture — the reason this is a no-op on all four charge types the form
	// offers rather than a new failure mode for three of them).
	if snapshot.EstimatedPrice == nil {
		return nil, fmt.Errorf("%s", missingWorkflowPriceMessage)
	}
	// The price the card shows is the snapshot's, verbatim — the same string that
	// gets sealed. It already carries 预估, because upstream cannot hold a price and
	// the frontend that renders this frame is not ours to relabel.
	price := snapshot.EstimatedPrice.DisplayText
	priceNote := createPriceNote
	summary := map[string]any{
		"workflow":   "CreateInstanceWorkflow",
		"GpuType":    draft.Args.GpuType,
		"Gpu":        draft.Args.GPU,
		"CPU":        draft.Args.CPU,
		"Memory":     draft.Args.Memory,
		"Zone":       zone,
		"ZoneLabel":  zoneDisplayLabel(wfCtx, zone),
		"ChargeType": draft.Args.ChargeType,
		"image":      draft.Image.Name,
		"price":      price,
		// Non-empty whenever a card exists at all, now that a priceless create stops
		// at the gate above. Kept additive (always present) for the renderers, which
		// still skip it when empty.
		"PriceNote": priceNote,
	}
	if name := strings.TrimSpace(draft.Args.Name); name != "" {
		summary["Name"] = name
	}
	if disk := createSystemDiskSummary(draft.Args.Disks); disk != "" {
		summary["SystemDisk"] = disk
	}
	if disk := createDataDiskSummary(draft.Args.Disks); disk != "" {
		summary["DataDisk"] = disk + "（实例进入运行状态后异步创建并挂载）"
	}
	return summary, nil
}

// createSystemDiskSummary renders the system disk already present in the sealed
// create draft. It never consults the catalog again: the card and the eventual
// request therefore describe the same disk contract.
func createSystemDiskSummary(disks []any) string {
	return createDiskSummary(disks, true)
}

func createDataDiskSummary(disks []any) string {
	return createDiskSummary(disks, false)
}

func createDiskSummary(disks []any, boot bool) string {
	for _, raw := range disks {
		disk, _ := raw.(map[string]any)
		if disk == nil || paramBool(disk, "IsBoot", false) != boot {
			continue
		}
		diskType := strings.TrimSpace(paramStr(disk, "Type", ""))
		if strings.EqualFold(diskType, deployment.DiskTypeCloudSSD) {
			if boot {
				diskType = "SSD 云盘"
			} else {
				diskType = "SSD 云数据盘"
			}
		}
		size, hasSize := createDiskSizeGB(disk["Size"])
		switch {
		case diskType != "" && hasSize:
			return fmt.Sprintf("%s %.0fGB", diskType, size)
		case diskType != "":
			return diskType
		case hasSize:
			return fmt.Sprintf("%.0fGB", size)
		default:
			return ""
		}
	}
	return ""
}

func createDiskSizeGB(raw any) (float64, bool) {
	switch size := raw.(type) {
	case uint32:
		return float64(size), size > 0
	case int:
		return float64(size), size > 0
	case float64:
		return size, size > 0
	default:
		return 0, false
	}
}

// stepCreateInstance executes the sealed draft and nothing else.
//
// Its BuildArgs deliberately reads ONE key. It does not call resolveTargetSpec,
// does not pick an image, does not consult "查询可用配比", and does not fill a
// default — because every one of those would be a decision made AFTER the user
// confirmed, outside the contract they approved. The draft was materialized and
// validated before the card was rendered; by the time this runs the only correct
// action is to send it verbatim.
func stepCreateInstance() Step {
	return Step{
		Name:      "创建实例",
		Type:      StepToolCall,
		Tool:      "CreateCompShareInstance",
		BuildArgs: createArgsFromSealedDraft,
	}
}

// createArgsFromSealedDraft returns the SEALED draft as the upstream request.
//
// It reads Context.sealed, not Context.Params. The distinction is the whole
// guarantee: Params is the live, mutable working set — a draft sitting there means
// only "someone computed one", never "the user approved it". sealed is written by
// Context.seal exactly when a confirmation gate PASSES, and unseal() voids it the
// moment a gate is re-entered, so `sealed != nil` is the only fact in this package
// that actually means "confirmed".
//
// Requiring it here matters because verifySealedContract fails OPEN on a nil seal
// (engine.go: `if wfCtx.sealed == nil || ...verifyDigest(...)` returns true). So
// had this function kept reading Params, a future reordering that put the create
// before its gate would have found a materialized draft, passed the digest check
// vacuously, and created an instance nobody confirmed. The current step order does
// not do that; this makes it structural rather than a property of the current
// list.
//
// A missing or unsealed draft is a hard error, never a re-derivation — silently
// rebuilding the arguments here is precisely the drift the draft replaced.
func createArgsFromSealedDraft(wfCtx *Context) (map[string]any, error) {
	snapshot, err := sealedCreateConfirmation(wfCtx)
	if err != nil {
		return nil, err
	}
	// A contract with no price cannot be a create anyone agreed to: the card that
	// forms the agreement cannot be built without one.
	if snapshot.EstimatedPrice == nil {
		return nil, fmt.Errorf("已确认的执行合同中没有价格记录，拒绝创建：用户不可能确认过一个没有价格的下单")
	}
	return snapshot.Execution.UpstreamCreateArgs(), nil
}

func sealedCreateConfirmation(wfCtx *Context) (CreateConfirmationSnapshot, error) {
	if wfCtx.sealed == nil || wfCtx.sealed.Operation != "CreateInstanceWorkflow" {
		return CreateConfirmationSnapshot{}, fmt.Errorf("创建实例缺少已确认的执行合同，拒绝以未经确认的参数创建")
	}
	stored, ok := wfCtx.sealed.BusinessParams[createDraftKey].(map[string]any)
	if !ok || len(stored) == 0 {
		return CreateConfirmationSnapshot{}, fmt.Errorf("已确认的执行合同中缺少创建参数，拒绝以重新推导的参数创建")
	}
	snapshot, err := ParseCreateConfirmationSnapshot(stored)
	if err != nil {
		return CreateConfirmationSnapshot{}, fmt.Errorf("已确认的执行合同无法解析（%v），拒绝以重新推导的参数创建", err)
	}
	return snapshot, nil
}

func paramStr(params map[string]any, key, defaultVal string) string {
	if v, ok := params[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return defaultVal
}

func paramBool(params map[string]any, key string, defaultVal bool) bool {
	if v, ok := params[key]; ok {
		switch b := v.(type) {
		case bool:
			return b
		case string:
			parsed, err := strconv.ParseBool(b)
			if err == nil {
				return parsed
			}
		}
	}
	return defaultVal
}

func paramNum(params map[string]any, key string, defaultVal float64) float64 {
	if v, ok := params[key]; ok {
		switch n := v.(type) {
		case float64:
			return n
		case int:
			return float64(n)
		}
	}
	return defaultVal
}

// pickImageId dispatches to the correct picker based on ImageSource.
//
// A caller may THREAD an already-resolved CompShareImageId in params (the
// deploy_model handler does, so the saga creates exactly the image the matcher chose +
// sized the GPU for, instead of re-resolving independently). ReAct callers do
// NOT set it, so their resolution is byte-unchanged.
// SelectedImage binds the executable ID and user-visible name to one catalog
// selection.
type SelectedImage struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Source string `json:"source"`
}

// selectCreateImage resolves the image once through the deterministic
// interpreter (deployment.ResolveImage) on the turn's image catalog. It is the only
// image decision in the create flow; the draft carries the result whole, so the
// confirm card renders Name and the create sends ID without either re-selecting.
// The resolver enforces two invariants:
//   - An explicitly threaded CompShareImageId (a 230-recovery re-run or a form
//     override) is VERIFIED against the catalog; only a verified id is sealed, with
//     its catalog name. An unverified id is NOT sealed under the caller's ImageName
//     and is never replaced by a name-ranked image.
//   - A named request with no exact catalog match is not silently swapped: the
//     resolver returns a ranked candidate whose REAL catalog name the confirm card
//     shows, and the user confirms it (the acceptance gate). Community no longer
//     blindly takes groups[0].Data[0].
func selectCreateImage(wfCtx *Context) SelectedImage {
	return resolveSelectedImage(wfCtx.Params, createImageCatalog(wfCtx))
}

// resolveSelectedImage is the shared image decision used by both the create seal
// (selectCreateImage) and the guided/confirm image forms — one interpreter, one
// snapshot, so the id a form offers as "current" is resolved the same way the
// create seals it.
//
// An explicitly threaded CompShareImageId is VERIFIED against the catalog; only a
// verified id wins with its catalog name. An unverified id fails closed and never
// falls through to name-based resolution: once the request names an exact object,
// substituting a ranked name match would make the card and create act on different
// images. A name-only request with no exact match may still return the best ranked
// recommendation (real catalog name), which rides the confirm gate. Prefiltered
// says whether the QUERY already applied the name, so a
// non-exact request recommends the best returned row rather than re-rejecting the
// API's own hits. Only the community query does that now (FuzzySearch=); the
// platform query stopped narrowing by name because upstream matches it
// case-sensitively — see stepQueryImages. Claiming prefiltered over an
// un-narrowed catalog would keep every unrelated row as a candidate.
func resolveSelectedImage(params map[string]any, snap *deployment.ImageCatalogSnapshot) SelectedImage {
	source := normalizedImageSource(paramStr(params, "ImageSource", imageSourcePlatform))
	if id := paramStr(params, "CompShareImageId", ""); id != "" {
		if res := deployment.ResolveImage(snap, deployment.ImageRequest{ID: id}); res.Status == deployment.ResolutionResolved {
			return selectedImageFrom(res.Selection, source)
		}
		return SelectedImage{Source: source}
	}
	res := deployment.ResolveImage(snap, deployment.ImageRequest{
		Name:         paramStr(params, "ImageName", ""),
		RequestedGPU: paramStr(params, "GpuType", ""),
		Zone: deployment.ZoneConstraint{
			Zone:  paramStr(params, "Zone", ""),
			IsPod: paramBool(params, "ZoneIsPod", false) || paramBool(params, "IsPodZone", false),
		},
		Source: source,
		// Community FuzzySearch narrows upstream; platform and custom list their
		// public/tenant catalogs and are ranked locally. Claiming prefiltered over
		// either un-narrowed catalog would keep unrelated rows as candidates.
		Prefiltered: source == imageSourceCommunity && strings.TrimSpace(paramStr(params, "ImageName", "")) != "",
	})
	if res.Status == deployment.ResolutionResolved {
		return selectedImageFrom(res.Selection, source)
	}
	if len(res.Candidates) > 0 {
		return selectedImageFrom(res.Candidates[0], source)
	}
	return SelectedImage{Source: source}
}

// formImageCatalog builds the snapshot the guided/confirm image forms rank from,
// detecting the response shape: a grouped CompshareImageGroup is community, a flat
// ImageSet is the requested platform/custom/shared source.
func formImageCatalog(images map[string]any, source string) *deployment.ImageCatalogSnapshot {
	if images == nil {
		return deployment.NewImageCatalogSnapshot(false, nil)
	}
	if _, ok := images["CompshareImageGroup"]; ok {
		return deployment.NewImageCatalogSnapshot(true, deployment.ParseCommunityImageEntries(images))
	}
	tag := normalizedImageSource(source)
	if tag == imageSourceCommunity {
		tag = imageSourcePlatform
	}
	return deployment.NewImageCatalogSnapshot(true, deployment.ParsePlatformImageEntries(images, tag))
}

// createImageCatalog is the workflow's single view of the image catalog for
// selection. THIS RUN's 查询镜像 remains authoritative for browsing, while an
// exact, resolver-verified threaded id is merged into it when the browse page did
// not contain that row.
//
// The engine's snapshot is taken at PROPOSAL time, against the source the
// proposal declared. The guided flow then lets the user change that source
// (选择镜像来源 → 查询镜像 re-query) and, when a name matched nothing, widens to
// the whole catalog (stepBrowseCommunityWhenNameMatchedNothing). Both produce a
// catalog that is strictly newer and matches the CURRENT ImageSource; a
// proposal-time snapshot that outranked them would silently show the user the
// images of a source they just switched away from.
//
// The merge is deliberately one-row and source-checked. It cannot reintroduce a
// catalog from a source the user switched away from; it only preserves identity
// for the exact id already verified this turn. Without it, a community suggestion
// outside the arbitrary 100-row browse page appeared on the picker but
// materializeCreateDraft later discarded it and selected another image by name.
//
// Either way there is one source per run, so the selection reads the SAME images
// the compatibility / boot-disk checks read, never a second catalog.
func createImageCatalog(wfCtx *Context) *deployment.ImageCatalogSnapshot {
	if result := createImageResult(wfCtx); result != nil {
		return formImageCatalog(result, normalizedImageSource(paramStr(wfCtx.Params, "ImageSource", imageSourcePlatform)))
	}
	if snap := wfCtx.ImageCatalog(); snap.Available() {
		return snap
	}
	return deployment.NewImageCatalogSnapshot(false, nil)
}

// createImageResult returns this run's raw image result, augmented with the one
// resolver-verified threaded image when the browse page omitted it. The original
// StepResult is never mutated; callers that need names, compatibility, GPU hints
// or disk size all read the same augmented view.
func createImageResult(wfCtx *Context) map[string]any {
	if wfCtx == nil {
		return nil
	}
	result := wfCtx.Result("查询镜像")
	id := strings.TrimSpace(paramStr(wfCtx.Params, "CompShareImageId", ""))
	if id == "" || imageMapByID(result, id) != nil {
		return result
	}
	entry, ok := wfCtx.ImageCatalog().ByID(id)
	if !ok || normalizedImageSource(entry.Source) !=
		normalizedImageSource(paramStr(wfCtx.Params, "ImageSource", "platform")) {
		return result
	}
	out := deepCopyParams(result)
	if out == nil {
		out = map[string]any{}
	}
	row := imageCatalogEntryAsResultRow(entry)
	if groups, grouped := out["CompshareImageGroup"].([]any); grouped ||
		(normalizedImageSource(entry.Source) == "community" && out["ImageSet"] == nil) {
		group := map[string]any{
			"ImageName": entry.Name,
			"Data":      []any{row},
		}
		if entry.FamilyID != "" {
			group["GroupId"] = entry.FamilyID
		}
		out["CompshareImageGroup"] = append([]any{group}, groups...)
		return out
	}
	imageSet, _ := out["ImageSet"].([]any)
	out["ImageSet"] = append([]any{row}, imageSet...)
	return out
}

func imageCatalogEntryAsResultRow(entry deployment.ImageCatalogEntry) map[string]any {
	name := entry.Name
	if entry.VersionName != "" {
		name = entry.VersionName
	}
	row := map[string]any{
		"CompShareImageId": entry.ID,
		"Name":             name,
		"ImageType":        entry.ImageType,
		"Status":           entry.Status,
		"Container":        strconv.FormatBool(entry.Container),
		"Size":             entry.SizeMB,
	}
	if entry.VersionName != "" {
		row["VersionName"] = entry.VersionName
	}
	if entry.Description != "" {
		row["Description"] = entry.Description
	}
	if len(entry.SupportedGPUTypes) > 0 {
		row["SupportedGpuTypes"] = stringsAsAny(entry.SupportedGPUTypes)
	}
	if len(entry.Tags) > 0 {
		row["Tags"] = stringsAsAny(entry.Tags)
	}
	return row
}

func stringsAsAny(values []string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

// selectedImageFrom projects a resolver ImageSelection onto the create flow's
// SelectedImage, keeping the declared source. A missing name shows as 未知 (the
// selection came from the catalog, so this is reached only for a truly nameless row).
func selectedImageFrom(sel deployment.ImageSelection, source string) SelectedImage {
	name := sel.Name
	if name == "" {
		name = "未知"
	}
	return SelectedImage{ID: sel.ID, Name: name, Source: source}
}

func pickImageId(params map[string]any, result map[string]any) string {
	if id := paramStr(params, "CompShareImageId", ""); id != "" {
		return id
	}
	return resolveSelectedImage(params, formImageCatalog(result, paramStr(params, "ImageSource", "platform"))).ID
}

func createImageUnavailableError(params map[string]any) error {
	imageName := strings.TrimSpace(paramStr(params, "ImageName", ""))
	if imageName != "" {
		return fmt.Errorf("未找到可用的 %s 镜像；候选镜像可能已下线或不适配当前实例形态，请换镜像或稍后重试", imageName)
	}
	return fmt.Errorf("未找到可用镜像，无法创建实例；请换镜像或稍后重试")
}

// Editable confirm form (v1, select-only).
//
// All option sets are assembled from data ALREADY collected by earlier steps
// (查询镜像 / 查询可用配比) — zero extra API calls, zero LLM. No stock or price
// claim is made for combinations that were never checked: after an override
// the 检查库存/查询价格 steps re-run and a refreshed card is re-confirmed
// (方案 A), so the authoritative answer always precedes creation.

const (
	maxFormGPUOptions     = 5
	maxFormImageOptions   = 3
	maxGuidedImageOptions = 10
	// The community endpoint does not honor tag or popularity ordering here, so
	// this is a broad classification page, not a "most popular" ranking.
	maxGuidedCommunityImageQueryLimit = 100
	// 100 is the upstream platform-catalog ceiling. TotalCount exposes future
	// truncation if the catalog grows beyond it.
	maxPlatformImageQueryLimit = 100
	// maxCustomImageQueryLimit is the documented ceiling of the tenant-scoped
	// DescribeCompShareCustomImages list. An exact candidate is additionally
	// verified against the engine's paginated tenant snapshot, so a row beyond this
	// browse page is never silently replaced by another image.
	maxCustomImageQueryLimit = 100
)

// createFormChargeTypes are the selectable billing modes. Postpay is the
// platform default; the deprecated Dynamic spelling is normalized away by
// createChargeType and never offered.
var createFormChargeTypes = []ConfirmFormOption{
	{Value: "Postpay", Label: "按量付费（按小时计费）"},
	{Value: "Spot", Label: "抢占式"},
	{Value: "Day", Label: "包日"},
	{Value: "Month", Label: "包月"},
}

// chargeTypeLabel renders a charge type for display, reusing the labels the
// selectable options already carry so the two can never drift into naming the
// same billing mode differently. An unknown value shows itself rather than being
// silently relabelled.
func chargeTypeLabel(chargeType string) string {
	for _, opt := range createFormChargeTypes {
		if strings.EqualFold(opt.Value, chargeType) {
			return opt.Label
		}
	}
	return chargeType
}

// createChargeTypeOptions gates Spot by zone for the PLAIN create's single
// confirm card, which resolves its zone before it asks and therefore can. The
// guided flow does the opposite — it asks the charge type first and gates the
// ZONE card by it (spotUnavailableInZone) — because a charge type asked last
// cannot inform the availability queries that already ran.
func createChargeTypeOptions(wfCtx *Context, zone string) []ConfirmFormOption {
	opts := make([]ConfirmFormOption, len(createFormChargeTypes))
	copy(opts, createFormChargeTypes)
	// If the zone can't be resolved here, show every charge type — the
	// authoritative create gate (validateCreatePlacement) reads the same pool
	// support fact and still refuses an unresolvable or unsupported pick.
	placement, err := workflowZonePlacement(wfCtx, zone)
	if err != nil {
		return opts
	}
	for i := range opts {
		pool := createInventoryPool(opts[i].Value)
		// Disable only on a KNOWN unsupported mode, matching validateCreatePlacement
		// exactly. Greying out an option we merely failed to confirm would hide a
		// mode the gate would have accepted — the mirror image of offering one it
		// will refuse, and just as wrong.
		if supported, known := createInventoryPoolSupport(wfCtx, placement, pool); known && !supported {
			opts[i].Disabled = true
			opts[i].Reason = "当前可用区和机型不支持" + createInventoryPoolLabel(pool) + "购买方式"
			opts[i].Note = opts[i].Reason
		}
	}
	return opts
}

// poolUnsupportedInZone reports whether the CURRENT charge type's purchase pool
// is KNOWN not to be sold for this model in this zone, and the label to show.
//
// An unresolvable placement or an unanswered backend is not evidence: the option
// stays enabled and validateCreatePlacement remains the authoritative refusal.
func poolUnsupportedInZone(wfCtx *Context, zone, gpuType string) (bool, string) {
	return poolUnsupportedInZoneForPool(wfCtx, zone, gpuType, createInventoryPool(createChargeType(wfCtx.Params)))
}

// imageContainerFit answers ONE narrow question: does the image's container/VM
// nature match the zone's kind? A pod zone runs container images only.
//
// It is deliberately not called "can this zone boot this image", which is a much
// larger question this type does NOT answer. It does not look at the image's
// Status, its SupportedGpuTypes, the zone's capacity, or the purchase mode — each
// of those is a separate check with its own call site, and imageContainerFitOK is
// therefore NOT a creatability proof. A reader who takes it for one will build the
// next gate on a guarantee that was never made.
//
// Within this axis it is shared by the zone card and authoritative create gate.
type imageContainerFit int

const (
	imageContainerFitOK imageContainerFit = iota
	// imageContainerFitNeedsContainerImage: a pod zone and a VM-only image. An
	// impossible pair, knowable as soon as the image is chosen.
	imageContainerFitNeedsContainerImage
	// imageContainerFitUnverifiable: a pod zone and an image this catalog page does
	// not carry (a community search can return a different page on a second query).
	// Distinct from NeedsContainerImage because it is OUR ignorance, not a known
	// mismatch — the card treats it as no reason to disable, while the create gate
	// refuses, because only the gate is entitled to refuse on missing evidence.
	imageContainerFitUnverifiable
)

func imageContainerFitForZone(images map[string]any, imageID string, placement deployment.ZonePlacement) imageContainerFit {
	if !placement.IsPod || strings.TrimSpace(imageID) == "" {
		return imageContainerFitOK
	}
	if imageMapByID(images, imageID) == nil {
		return imageContainerFitUnverifiable
	}
	if imageContainerByID(images, imageID) {
		return imageContainerFitOK
	}
	return imageContainerFitNeedsContainerImage
}

// zoneRejectsSelectedImage is the zone card's view of that verdict: it disables a
// zone only on a known mismatch.
//
// It does not claim the card and the gate can never disagree about an OUTCOME:
// imageZoneUnverifiable is deliberately passed here and refused there, and the
// combined stand-down in guidedZoneFormOptions can hand back a zone the gate will
// reject. What it does guarantee is that neither side invents its own rule.
func zoneRejectsSelectedImage(wfCtx *Context, zone string) (bool, string) {
	placement, err := workflowZonePlacement(wfCtx, zone)
	if err != nil {
		return false, ""
	}
	imageID := paramStr(wfCtx.Params, "CompShareImageId", "")
	if imageContainerFitForZone(createImageResult(wfCtx), imageID, placement) != imageContainerFitNeedsContainerImage {
		return false, ""
	}
	return true, "所选镜像不是容器镜像，该可用区用不了"
}

func poolUnsupportedInZoneForPool(wfCtx *Context, zone, gpuType, pool string) (bool, string) {
	placement, err := workflowZonePlacement(wfCtx, zone)
	if err != nil {
		return false, ""
	}
	supported, known := createInventoryPoolSupportFor(wfCtx, placement, gpuType, pool)
	if !known || supported {
		return false, ""
	}
	return true, "该可用区不支持" + createInventoryPoolLabel(pool) + "购买方式"
}

// chargeTypeUnsupportedInCatalog asks the widest version of the same question,
// for the card that runs before any GPU is chosen: is there NO model/zone pair
// in the whole catalog that sells this purchase mode?
//
// It has to be that weak. The charge type is a user preference, not a capability
// answer, and the narrowing happens on the two cards after it — which now read
// the chosen mode. Disabling here on anything less than "nowhere at all" would
// remove a mode some later combination could still buy.
func chargeTypeUnsupportedInCatalog(wfCtx *Context, chargeType string) bool {
	rows, _ := wfCtx.Result("查询可用配比")["AvailableInstanceTypes"].([]any)
	if len(rows) == 0 {
		return false
	}
	pool := createInventoryPool(chargeType)
	sawOne := false
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		name, _ := row["Name"].(string)
		zone, _ := row["Zone"].(string)
		if name == "" || zone == "" {
			continue
		}
		if status, _ := row["Status"].(string); status != "" && !strings.EqualFold(status, "Normal") {
			continue
		}
		sawOne = true
		if unsupported, _ := poolUnsupportedInZoneForPool(wfCtx, zone, name, pool); !unsupported {
			return false
		}
	}
	return sawOne
}

// poolUnsupportedEverywhere reports whether the current charge type's pool is
// known not to be sold for this model in EVERY zone the model is offered in.
//
// The GPU card comes before the zone card, so a model spans several zones here
// and only the aggregate is answerable. It has to be the strict one: a single
// zone that still sells the mode — or that the backend did not answer for —
// leaves the model buyable, and greying it out would remove a choice the zone
// card was about to make available. No API calls; the snapshot is already read.
func poolUnsupportedEverywhere(wfCtx *Context, zones []string, gpuType string) (bool, string) {
	if len(zones) == 0 {
		return false, ""
	}
	reason := ""
	for _, zone := range zones {
		unsupported, zoneReason := poolUnsupportedInZone(wfCtx, zone, gpuType)
		if !unsupported {
			return false, ""
		}
		if reason == "" {
			reason = zoneReason
		}
	}
	pool := createInventoryPool(createChargeType(wfCtx.Params))
	return true, "该机型不支持" + createInventoryPoolLabel(pool) + "购买方式"
}

func hasExplicitImageIntent(params map[string]any) bool {
	if strings.TrimSpace(paramStr(params, "CompShareImageId", "")) != "" {
		return true
	}
	if strings.TrimSpace(paramStr(params, "ImageName", "")) != "" {
		return true
	}
	if source := normalizedImageSource(paramStr(params, "ImageSource", imageSourcePlatform)); source == imageSourceCommunity || source == imageSourceCustom || source == imageSourceSharing {
		return true
	}
	return false
}

// buildCreateConfirmForm assembles the editable selection form shown with the
// create confirm card. Fields with no real alternative (single option) are
// omitted — the read-only Summary already displays their values.
func buildCreateConfirmForm(wfCtx *Context) (*ConfirmForm, error) {
	_, _, _, zone, err := resolveTargetSpec(wfCtx)
	if err != nil {
		return nil, err
	}
	gpuType, _ := wfCtx.Params["GpuType"].(string)
	catalog := wfCtx.Result("查询可用配比")
	images := createImageResult(wfCtx)

	supported := currentImageSupportedGPUs(wfCtx.Params, images)

	var fields []ConfirmFormField
	if opts := gpuFormOptions(catalog, supported, gpuType); len(opts) > 1 {
		fields = append(fields, ConfirmFormField{
			Key: "GpuType", Label: "GPU 型号", Type: "select",
			Value: gpuType, Editable: true, Options: opts,
		})
	}
	if opts := zoneFormOptions(wfCtx, catalog, gpuType, zone); len(opts) > 1 {
		fields = append(fields, ConfirmFormField{
			Key: "Zone", Label: "可用区", Type: "select",
			Value: zone, Editable: true, Options: opts,
		})
	}
	if cur, opts := imageFormOptions(wfCtx.Params, images, gpuType, createImageTaxonomy(wfCtx)); cur != "" && len(opts) > 1 {
		fields = append(fields, ConfirmFormField{
			Key: "ImageId", Label: "镜像", Type: "select",
			Value: cur, Editable: true, Options: opts,
		})
	}
	fields = append(fields, ConfirmFormField{
		Key: "ChargeType", Label: "计费方式", Type: "select",
		Value: createChargeType(wfCtx.Params), Editable: true, Options: createChargeTypeOptions(wfCtx, zone),
	})
	return &ConfirmForm{Version: 1, Fields: fields}, nil
}

// shouldSkipSourceReQuery skips the post-source re-query when an explicit image is
// pinned (no browsing) or when the guided source step did NOT change the source from the
// initial one — then the first 查询镜像 already fetched the right source and its result
// is authoritative. When the source DID change (either direction — platform↔community),
// the re-query replaces the stale initial catalog with the chosen source's, so the
// facets/picker/resolve steps never read a foreign-source listing.
func shouldSkipSourceReQuery(wfCtx *Context) (bool, error) {
	if strings.TrimSpace(paramStr(wfCtx.Params, "CompShareImageId", "")) != "" {
		return true, nil
	}
	return normalizedImageSource(paramStr(wfCtx.Params, "ImageSource", "platform")) ==
		normalizedImageSource(paramStr(wfCtx.InitialParams, "ImageSource", "platform")), nil
}

func initialParamSet(wfCtx *Context, key string) bool {
	if wfCtx == nil || wfCtx.InitialParams == nil {
		return false
	}
	_, ok := wfCtx.InitialParams[key]
	return ok
}

func enabledOptionExists(opts []ConfirmFormOption, value string) bool {
	for _, opt := range opts {
		if !opt.Disabled && strings.EqualFold(opt.Value, value) {
			return true
		}
	}
	return false
}

func isOnlyEnabledOption(opts []ConfirmFormOption, value string) bool {
	selectable := 0
	for _, opt := range opts {
		if opt.Disabled {
			continue
		}
		if opt.Value != value {
			return false
		}
		selectable++
	}
	return selectable == 1
}
