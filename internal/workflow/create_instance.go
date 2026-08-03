package workflow

import (
	"fmt"
	"sort"
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
	// These are workflow-internal evidence steps. The first freezes the image
	// phrase derived from the initial live catalog before a source choice can
	// replace 查询镜像; the second checks only the opposite live catalog. Keeping
	// them separate lets later cards distinguish "not found there" from "we never
	// checked there" without writing an inferred source or image into Params.
	imageCatalogIntentStepName    = "识别镜像意图"
	alternateImageCatalogStepName = "核验另一镜像目录"
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
		// No Description: after cede00d4 the model-facing text for this operation is
		// the tool's own description (tools/registry.go -> capability.AgentInstruction
		// -> actionresolver/catalog.go AgentDescription -> engine/dispatch_window.go).
		// A Description here would be dead text at best and a second, drifting source
		// of the trigger rule at worst. The step list that used to live here was
		// removed for a separate reason: it described the server's own pipeline and
		// primed the Agent to re-run those reads itself.
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
		// CPU-memory / image source+selection / charge type). Name is accepted from
		// an explicit user proposal and sealed below, but remains optional rather
		// than turning the guided flow into an extra question for most users.
		GuidedIntakeFields: []string{"GpuType", "Zone", "Gpu", "Cpu", "Memory", "ImageSource", "ImageName", "ChargeType"},
		// Name is cosmetic and the platform can generate one, so an invalid value
		// may be dropped. CompShareImageId is intentionally NOT discardable: once a
		// request names an exact image, silently replacing a stale/wrong-source id
		// with an unrelated browse result changes the requested object.
		DiscardableOnRejectFields: []string{"Name"},
	}
}

// CreateInstanceGuidedDef returns the guided, Figma-style order flow for
// creating a CompShare GPU instance. The public action name stays
// CreateInstanceWorkflow so old tooling and confirmation labels remain stable.
func CreateInstanceGuidedDef() *Definition {
	return &Definition{
		// No Description here either: it was a narration of the step list below, which
		// drifts the moment a step moves — and this flow's order has now changed twice.
		// The steps are the description.
		Name: "CreateInstanceWorkflow",
		Steps: []Step{
			stepQueryImages(true),
			stepResolveImageCatalogIntent(),
			stepQueryAlternateImageCatalog(),
			// The legal machine catalog is not charge-type scoped. It may therefore be
			// fetched in the background before the user chooses billing; the capacity
			// calls that actually depend on billing remain below the charge-type card.
			stepQueryInstanceTypes(),
			// GPU inventory comes from TWO upstream implementations: the request's
			// zone_id selects the backend rather than filtering the result, so an
			// absent zone_id reaches only the official pools and a Pod zone's stock
			// reads as a real zero. cede00d4 splits the call and merges the answers
			// against the live zone catalog; this flow takes that whole mechanism.
			stepQueryOfficialGPUInventory(),
			stepQueryPodGPUInventory(),
			stepResolveGPUInventorySnapshot(),
			// The CARD order stays image-first (this branch's reordering), which
			// cede00d4 predates rather than disagrees with: it branched from the
			// hardware-first order and only inserted the inventory steps above.
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
			// One capacity fan-out over every (model, zone) the catalog offers, read
			// by BOTH hardware cards. It used to sit between them and cover only the
			// chosen model's zones; the GPU card was then the one place a user could
			// click something they could not buy. Image and charge type are settled by
			// here, which is everything the call needs except the zone.
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

// ---------------------------------------------------------------------------
// Step definitions (params aligned with docs/api/ specs)
// ---------------------------------------------------------------------------

func stepQueryImages(allowCommunityBrowse bool) Step {
	return Step{
		Name: "查询镜像",
		Type: StepToolCall,
		ToolFunc: func(wfCtx *Context) string {
			if paramStr(wfCtx.Params, "ImageSource", "platform") == "community" {
				return "DescribeCommunityImages"
			}
			return "DescribeCompShareImages"
		},
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			if paramStr(wfCtx.Params, "ImageSource", "platform") == "community" {
				if id := strings.TrimSpace(paramStr(wfCtx.Params, "CompShareImageId", "")); id != "" {
					// A user-pinned id and every plain-flow id need one exact row. An
					// Agent-suggested community id is different: the picker remains a
					// real confirmation, but its alternatives are the upstream-declared
					// versions of THIS image family, never an unrelated catalog page.
					if imageUserSettled(wfCtx) || !allowCommunityBrowse {
						return communityImageExactArgs(id), nil
					}
					return suggestedCommunityImageQueryArgs(wfCtx, id), nil
				}
				name := paramStr(wfCtx.Params, "ImageName", "")
				if name == "" {
					if allowCommunityBrowse {
						return communityImageBrowseArgs(""), nil
					}
					return nil, fmt.Errorf("使用社区镜像创建实例时必须指定镜像名称（ImageName），请告诉我您想使用哪个社区镜像")
				}
				if allowCommunityBrowse && wfCtx.ImageSelection() == ImageSelectionSuggested {
					return communityImageBrowseArgs(""), nil
				}
				return map[string]any{"FuzzySearch": name}, nil
			}
			args := map[string]any{
				"Limit": maxPlatformImageQueryLimit,
			}
			// Narrow the query ONLY when the user settled the image. A settled id
			// skips the picker, so the by-id row is all capacity/price needs; a
			// settled name narrows a browse the user asked for. An Agent SUGGESTION
			// the user has not chosen must NOT narrow the catalog — the picker still
			// runs and needs the whole catalog to offer alternatives, with the
			// suggested id preselected from within it (a one-row "choice" card,
			// narrowed by the Agent's own id, was the bug).
			if imageUserSettled(wfCtx) || !allowCommunityBrowse {
				if id := paramStr(wfCtx.Params, "CompShareImageId", ""); id != "" {
					args["CompShareImageId"] = id
					return args, nil
				}
			}
			// A settled NAME deliberately does not narrow the query, although an id
			// does. Upstream matches Name case-sensitively — measured live on the
			// platform catalog: no Name = 75 rows, "pytorch" = 7, "PyTorch" = 1,
			// "Pytorch" = 0. And the Agent cannot spell it any other way than the
			// user did: a slot only earns SourceUserExplicit by being a verbatim span
			// of the user's message, so "用最新Pytorch镜像" can only ever produce
			// Name="Pytorch". Narrowing on it returned an empty catalog and the
			// picker died with 未找到可选镜像 — the more faithfully the Agent quoted
			// the user, the more certainly the query found nothing.
			//
			// The whole catalog costs one larger response and is what the ranker was
			// written for: nameSimilarity lowercases both sides, and rankRecommendations
			// tiebreaks the same framework by its structured version (using the
			// upstream index when populated), so "最新pytorch" lands the newest
			// PyTorch at the top of the picker.
			return args, nil
		},
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

// suggestedCommunityImageQueryArgs collects only the family corpus that can
// legitimately appear beside an Agent's exact recommendation. DescribeCommunityImages
// has no GroupId request parameter, so it searches the source-provided family label;
// recommendedCommunityImageScope then checks the returned GroupId locally before any
// card reads it. A missing family label deliberately falls back to the exact row — it
// must never widen to arbitrary same-name or unrelated community images.
func suggestedCommunityImageQueryArgs(wfCtx *Context, id string) map[string]any {
	scope, ok := currentRecommendedCommunityImageScope(wfCtx)
	if !ok || !scope.hasFamily || strings.TrimSpace(scope.familyQuery) == "" {
		return communityImageExactArgs(id)
	}
	return communityImageBrowseArgs(scope.familyQuery)
}

// imageCatalogIntentSeed is the evidence captured from the FIRST live catalog
// before the source card can replace 查询镜像 with the other source. Query is the
// catalog-derived/user-named phrase used to check the opposite catalog; Request is
// the structured request that ranks the initial source without turning the phrase
// into a concrete image selection.
type imageCatalogIntentSeed struct {
	Query          string
	InitialSource  string
	Request        deployment.ImageRequest
	InitialMatches int
	// Structured is true only when Query came from a framework/tag literally
	// present in both the user's text and the live initial catalog. False means
	// Query is free-form ImageName text proposed by the model or copied by the
	// user; a community FuzzySearch miss for that wording is never absence proof.
	Structured bool
}

// stepResolveImageCatalogIntent freezes the current turn's image phrase as a
// read-only candidate. It writes only StepResults: no source, name or id is added
// to Params, so a catalog match cannot silently become user authorization.
func stepResolveImageCatalogIntent() Step {
	return Step{
		Name: imageCatalogIntentStepName,
		Type: StepResolve,
		SkipIf: func(wfCtx *Context) (bool, error) {
			_, ok := deriveImageCatalogIntentSeed(wfCtx)
			return !ok, nil
		},
		Resolve: func(wfCtx *Context) (map[string]any, error) {
			seed, ok := deriveImageCatalogIntentSeed(wfCtx)
			if !ok {
				return map[string]any{}, nil
			}
			return encodeImageCatalogIntentSeed(seed), nil
		},
	}
}

// stepQueryAlternateImageCatalog asks only the opposite live catalog whether the
// same image phrase exists there. It runs solely while source is unresolved and
// only when the initial catalog produced a bounded query. A failure is optional
// enrichment, but absence of its StepResult means UNKNOWN and therefore keeps the
// source card — a failed check can never be interpreted as "no match".
func stepQueryAlternateImageCatalog() Step {
	return Step{
		Name:     alternateImageCatalogStepName,
		Type:     StepToolCall,
		Optional: true,
		SkipIf: func(wfCtx *Context) (bool, error) {
			if wfCtx == nil || wfCtx.ImageSourceUserPinned() ||
				strings.TrimSpace(paramStr(wfCtx.Params, "CompShareImageId", "")) != "" ||
				guidedStepWasReached(wfCtx, guidedStepImageSource) {
				return true, nil
			}
			_, ok := storedImageCatalogIntentSeed(wfCtx)
			return !ok, nil
		},
		ToolFunc: func(wfCtx *Context) string {
			seed, _ := storedImageCatalogIntentSeed(wfCtx)
			if oppositeImageSource(seed.InitialSource) == "community" {
				return "DescribeCommunityImages"
			}
			return "DescribeCompShareImages"
		},
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			seed, ok := storedImageCatalogIntentSeed(wfCtx)
			if !ok {
				return nil, fmt.Errorf("缺少可核验的镜像意图")
			}
			if oppositeImageSource(seed.InitialSource) == "community" {
				return communityImageBrowseArgs(seed.Query), nil
			}
			// Platform Name filtering is case-sensitive and can turn a valid user
			// spelling into an empty response. Its catalog fits in one 100-row call,
			// so fetch it whole and apply the same local generic matcher as the picker.
			return map[string]any{"Limit": maxPlatformImageQueryLimit}, nil
		},
	}
}

func deriveImageCatalogIntentSeed(wfCtx *Context) (imageCatalogIntentSeed, bool) {
	if wfCtx == nil ||
		strings.TrimSpace(paramStr(wfCtx.Params, "CompShareImageId", "")) != "" {
		return imageCatalogIntentSeed{}, false
	}
	source := normalizedImageSource(paramStr(wfCtx.Params, "ImageSource", "platform"))
	snap := formImageCatalog(wfCtx.Result("查询镜像"), source)
	if !snap.Available() {
		return imageCatalogIntentSeed{}, false
	}

	name := strings.TrimSpace(paramStr(wfCtx.Params, "ImageName", ""))
	request, inferred := deployment.InferImageCatalogRequest(snap, wfCtx.ImageIntentText(), source)
	if inferred && wfCtx.ImageSelection() == ImageSelectionUserPinned && name != "" {
		// A specific name such as "Acme PyTorch Workbench" must be checked as that
		// image, not broadened to every PyTorch row merely because it contains the
		// framework word. An exact bare catalog fact ("PyTorch") remains structured.
		exactCatalogFact := (request.Framework != "" && strings.EqualFold(name, strings.TrimSpace(request.Framework))) ||
			(request.Tag != "" && strings.EqualFold(name, strings.TrimSpace(request.Tag)))
		if !exactCatalogFact {
			inferred = false
		}
	}

	query := ""
	if inferred {
		query = strings.TrimSpace(request.Framework)
		if query == "" {
			query = strings.TrimSpace(request.Tag)
		}
	} else if name != "" {
		request = deployment.ImageRequest{Name: name, Source: source}
		query = name
	}
	if query == "" {
		return imageCatalogIntentSeed{}, false
	}
	request.Source = source
	return imageCatalogIntentSeed{
		Query:          query,
		InitialSource:  source,
		Request:        request,
		InitialMatches: len(deployment.RankImages(snap, request)),
		Structured:     inferred,
	}, true
}

func encodeImageCatalogIntentSeed(seed imageCatalogIntentSeed) map[string]any {
	return map[string]any{
		"Query":          seed.Query,
		"InitialSource":  seed.InitialSource,
		"Name":           seed.Request.Name,
		"Framework":      seed.Request.Framework,
		"Tag":            seed.Request.Tag,
		"InitialMatches": seed.InitialMatches,
		"Structured":     seed.Structured,
	}
}

func storedImageCatalogIntentSeed(wfCtx *Context) (imageCatalogIntentSeed, bool) {
	if wfCtx == nil {
		return imageCatalogIntentSeed{}, false
	}
	result := wfCtx.Result(imageCatalogIntentStepName)
	query := strings.TrimSpace(paramStr(result, "Query", ""))
	source := normalizedImageSource(paramStr(result, "InitialSource", "platform"))
	if query == "" {
		return imageCatalogIntentSeed{}, false
	}
	return imageCatalogIntentSeed{
		Query:         query,
		InitialSource: source,
		Request: deployment.ImageRequest{
			Name:      paramStr(result, "Name", ""),
			Framework: paramStr(result, "Framework", ""),
			Tag:       paramStr(result, "Tag", ""),
			Source:    source,
		},
		InitialMatches: int(paramNum(result, "InitialMatches", 0)),
		Structured:     paramBool(result, "Structured", false),
	}, true
}

// currentImageCatalogIntentSeed prefers the frozen initial evidence but remains
// usable in focused unit/plain contexts that have a catalog and no resolve step.
func currentImageCatalogIntentSeed(wfCtx *Context) (imageCatalogIntentSeed, bool) {
	if seed, ok := storedImageCatalogIntentSeed(wfCtx); ok {
		return seed, true
	}
	return deriveImageCatalogIntentSeed(wfCtx)
}

func oppositeImageSource(source string) string {
	if normalizedImageSource(source) == "community" {
		return "platform"
	}
	return "community"
}

// alternateImageCatalogMatchCount returns checked=false when the optional probe
// did not produce a result, or when a free-form phrase produced a community
// FuzzySearch zero. Both states are unknown and keep the source choice visible.
// A literal framework/tag recovered from the live initial catalog is structured
// source evidence and may remain settled after its opposite literal probe misses.
func alternateImageCatalogMatchCount(wfCtx *Context, seed imageCatalogIntentSeed) (count int, checked bool) {
	if wfCtx == nil {
		return 0, false
	}
	result, checked := wfCtx.StepResults[alternateImageCatalogStepName]
	if !checked {
		return 0, false
	}
	source := oppositeImageSource(seed.InitialSource)
	snap := formImageCatalog(result, source)
	request := deployment.ImageRequest{Name: seed.Query, Source: source}
	if matches := len(deployment.RankImages(snap, request)); matches > 0 {
		// A positive FuzzySearch result is enough to keep the source choice visible.
		return matches, true
	}
	if source != "community" {
		// Platform alternate checks fetch the whole (currently sub-100 row)
		// catalog, so a successful zero is a real local-ranking zero.
		return 0, true
	}

	if !seed.Structured {
		// The upstream community "fuzzy" filter is whole-phrase containment.
		// A verbose free-form name can miss a related family solely because of
		// wording, so its zero is unknown and must keep the source choice visible.
		return 0, false
	}
	// A structured query is a literal framework/tag recovered from the user's
	// words and the live initial catalog, not model-authored prose. A successful
	// opposite-source probe with no same literal name leaves that typed catalog
	// intent on its initial source without scanning the entire community catalog.
	return 0, true
}

// catalogIntentUniquelySettlesCurrentSource is true only after the initial source
// has matching live evidence and the opposite-source check found no conflict it
// can support. A match in both catalogs is a source choice, not permission to keep
// whichever default the Agent happened to emit; a free-form community miss stays
// unknown rather than being promoted to a directory-level absence claim.
func catalogIntentUniquelySettlesCurrentSource(wfCtx *Context) bool {
	seed, ok := currentImageCatalogIntentSeed(wfCtx)
	if !ok || seed.InitialMatches == 0 ||
		normalizedImageSource(paramStr(wfCtx.Params, "ImageSource", "platform")) != seed.InitialSource {
		return false
	}
	alternateMatches, checked := alternateImageCatalogMatchCount(wfCtx, seed)
	return checked && alternateMatches == 0
}

func imageCatalogIntentQuery(wfCtx *Context) string {
	if name := strings.TrimSpace(paramStr(wfCtx.Params, "ImageName", "")); name != "" {
		return name
	}
	if seed, ok := currentImageCatalogIntentSeed(wfCtx); ok {
		return seed.Query
	}
	return ""
}

// stepReQuerySelectedSourceImages re-fetches the image catalog for the source the user
// chose in the guided source step, into the SAME "查询镜像" result the whole image
// selection reads — so a source switch in EITHER direction (platform↔community) replaces
// the initial catalog with the chosen source's, and the facets/picker/resolve/boot-disk
// steps never read a stale foreign-source catalog. Skipped when the source is unchanged
// from the initial (the first 查询镜像 already fetched it) or an explicit image is pinned.
func stepReQuerySelectedSourceImages() Step {
	return Step{
		Name:   "查询镜像",
		Type:   StepToolCall,
		SkipIf: shouldSkipSourceReQuery,
		ToolFunc: func(wfCtx *Context) string {
			if paramStr(wfCtx.Params, "ImageSource", "platform") == "community" {
				return "DescribeCommunityImages"
			}
			return "DescribeCompShareImages"
		},
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			if paramStr(wfCtx.Params, "ImageSource", "platform") == "community" {
				return communityImageBrowseArgs(imageCatalogIntentQuery(wfCtx)), nil
			}
			args := map[string]any{"Limit": maxPlatformImageQueryLimit}
			if name := paramStr(wfCtx.Params, "ImageName", ""); name != "" {
				args["Name"] = name
			}
			return args, nil
		},
	}
}

// stepBrowseCommunityWhenNameMatchedNothing rescues a named community create whose
// upstream search matched nothing. ImageName is only the wording that happened to reach
// us — the Agent rewrites it freely between turns — and stepQueryImages passes it
// straight through as FuzzySearch, so a miss UPSTREAM leaves the catalog empty and the
// picker dies with "未找到可选社区镜像" while the image sits on the platform. Observed
// live 2026-07-21 on "用最强AI数字人 InfiniteTalk为我创建一台机器"; the same session's
// "用 InfiniteTalk 镜像创建一台实例" returned 22 rows, so the miss is a property of the
// wording, not of the catalog. A search miss must degrade to browsing, never to a dead
// end — no local fallback can do this, because by then the catalog is already empty.
//
// It re-queries WITHOUT the name and overwrites 查询镜像 in place, the same overwrite
// stepReQuerySelectedSourceImages performs. A narrowed-but-non-empty result is a useful
// narrowing and is deliberately left alone.
func stepBrowseCommunityWhenNameMatchedNothing() Step {
	return Step{
		Name: "查询镜像",
		Type: StepToolCall,
		Tool: "DescribeCommunityImages",
		SkipIf: func(wfCtx *Context) (bool, error) {
			if normalizedImageSource(paramStr(wfCtx.Params, "ImageSource", "platform")) != "community" {
				return true, nil
			}
			if strings.TrimSpace(imageCatalogIntentQuery(wfCtx)) == "" {
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
			// Deliberately NOT scoped to the Spot pool. InstanceType=spot looks
			// like the right way to ask "what can I buy on Spot", and upstream
			// accepts it — then returns nothing. DescribeAvailableCompShareInstanceTypes
			// appends a row only for InstanceType uhost/all (uhost-compshare-api,
			// ucloud/describe_available_compshare_instance_types.go formatResponse),
			// and its dispatcher has no Pod branch, so "spot" is a valid value with
			// an empty answer.
			//
			// Measured live 2026-07-22: rows=19 / 12 GPU types for absent, "uhost"
			// and "all"; rows=0 for "spot". An empty catalog makes resolveTargetSpec
			// fail with "未找到 X × N 卡的可用配比" listing no GPU types at all, so
			// sending it breaks every Spot create.
			//
			// Spot eligibility comes from DescribeCompShareGpuInventory instead: it
			// carries BOTH pools plus SpotUnsupportedGpuTypes, and needs no charge
			// type to ask.
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
	args := deployment.BuildCapacityArgs(deployment.DeploymentDraft{
		Zone:               zone,
		GPUType:            gpuType,
		CompShareImageID:   imageID,
		ChargeType:         createChargeType(wfCtx.Params),
		Disks:              workflowSystemDisks(wfCtx, imageID, zone, gpuType),
		MinimalCPUPlatform: workflowMinimalCPUPlatform(wfCtx, gpuType, zone),
	})
	return deployment.ApplyCapacityPlacementArgs(args, placement), true
}

const zoneCapacityStepName = "查询各可用区容量"

// stepProbeZoneCapacity asks, once per candidate zone, whether the resolved
// image + GPU can actually be created there — BEFORE the zone card is built, so
// a zone with no capacity is offered disabled instead of accepted and then
// refused eight steps later at 检查库存.
//
// It cannot be one call. The capacity API takes the zone as INPUT, so a single
// request only ever describes the zone already chosen; that is what
// stepQueryCapacitySpecs does, and it is why the sold-out answer used to arrive
// after the user had picked.
//
// It now covers the GPU card too, which an earlier version of this comment
// called impossible — "there the zone is not yet chosen". That was written when
// the GPU card came FIRST. Under the image-first order the image is already
// resolved here, and the image is the input the capacity call is hardest to
// assemble; only the zone is missing, and each model is offered in a handful of
// zones. So the fan-out is one call per (model, zone) row of the catalog — ~19
// live, not N_gpu × N_zone — and both cards read the one answer.
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
// catalog offers. When the GPU is already pinned it narrows to that model, so a
// flow that only needs the zone card still costs what it used to.
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
		for _, zone := range guidedCandidateZones(catalog, gpuType) {
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

// guidedCandidateGPUModels lists the models the catalog offers, in catalog
// order, deduplicated — the same enumeration the GPU card renders, kept as one
// function so the probe cannot ask about a different set than the card shows.
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

// guidedCandidateZones lists the zones the catalog offers this GPU in, in
// catalog order and deduplicated. It is the same enumeration guidedZoneFormOptions
// renders, kept as one function so the probe cannot ask about a different set of
// zones than the card shows — a zone present on the card but absent from the
// probe would render as unknown forever.
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

// stepQueryImageTagCatalog fetches the platform's own image classification so the
// filter card can offer 用途 categories instead of the raw tag strings that happen
// to appear on this page of the catalog.
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
			return imageUserSettled(wfCtx), nil
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

// imageCandidateSet is the ONE candidate set the whole image flow reads. Every
// number a card states and every option it offers is a projection of it, so the
// card cannot promise a population the next card does not have.
//
// It exists because they used to be computed twice. The facet card counted
// snap.Entries() — the raw catalog — while the picker ran the ranker (hard status
// and pod/container gates, plus the agent's structured request) and only then
// applied the facets. The card therefore advertised "框架 / 应用镜像 55 个镜像"
// against a picker that could hand back ten, and on a Pod zone it counted VM-only
// images the picker had already dropped.
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

// recommendedCommunityImageScope is a derived view of an Agent-proposed exact
// community image. It is not workflow state and is never written to Params: every
// invocation recomputes it from this turn's verified CompShareImageId and the
// upstream family facts attached to that exact catalog row.
//
// A family key is used only when upstream actually supplied a group identity. A
// community endpoint can occasionally omit group metadata; in that case exactID is
// the honest scope. We never infer family membership from overlapping display names.
type recommendedCommunityImageScope struct {
	exactID     string
	familyKey   string
	familyQuery string
	hasFamily   bool
}

// recommendedCommunityImageScope returns the candidate boundary for the single
// concrete image the Agent chose to carry into THIS create proposal. The Agent remains
// the conversation interpreter: when the user rejects or changes a prior
// recommendation, it simply omits that id (or supplies a different verified one) in
// the new proposal and this scope does not exist.
func currentRecommendedCommunityImageScope(wfCtx *Context) (recommendedCommunityImageScope, bool) {
	if wfCtx == nil || imageUserSettled(wfCtx) ||
		wfCtx.ImageSelection() != ImageSelectionSuggested ||
		normalizedImageSource(paramStr(wfCtx.Params, "ImageSource", "platform")) != "community" {
		return recommendedCommunityImageScope{}, false
	}
	id := strings.TrimSpace(paramStr(wfCtx.Params, "CompShareImageId", ""))
	if id == "" {
		return recommendedCommunityImageScope{}, false
	}
	entry, ok := wfCtx.ImageCatalog().ByID(id)
	if !ok || normalizedImageSource(entry.Source) != "community" {
		return recommendedCommunityImageScope{}, false
	}

	scope := recommendedCommunityImageScope{exactID: entry.ID}
	if strings.TrimSpace(entry.FamilyID) == "" && strings.TrimSpace(entry.FamilyName) == "" {
		return scope, true
	}
	scope.familyKey = entry.FamilyKey()
	scope.familyQuery = entry.FamilyLabel()
	scope.hasFamily = scope.familyKey != "" && scope.familyQuery != ""
	return scope, true
}

func (scope recommendedCommunityImageScope) contains(snap *deployment.ImageCatalogSnapshot, id string) bool {
	if scope.familyKey == "" {
		return strings.EqualFold(strings.TrimSpace(scope.exactID), strings.TrimSpace(id))
	}
	entry, ok := snap.ByID(id)
	return ok && entry.FamilyKey() == scope.familyKey
}

func scopeImageCandidateSet(set imageCandidateSet, scope recommendedCommunityImageScope) imageCandidateSet {
	keep := func(sel deployment.ImageSelection) bool {
		return scope.contains(set.snap, sel.ID)
	}
	set.base = filterSelections(set.base, keep)
	set.afterType = filterSelections(set.afterType, keep)
	set.final = filterSelections(set.final, keep)
	return set
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

// buildImageCandidateSetForRequest is the shared implementation for the ordinary
// proposal path and the current-turn catalog fallback. The latter supplies a
// structured Framework/Tag read from the live catalog instead of fabricating an
// ImageName; both paths still rank through deployment.RankImages.
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
// same authoritative pod flag, resolved from the zone catalog rather than the
// ZoneIsPod cache the picker used to read before it was warm.
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
	scope, hasRecommendedCommunityScope := currentRecommendedCommunityImageScope(wfCtx)
	if hasRecommendedCommunityScope {
		// The exact recommendation already determines the family boundary. Keeping
		// the Agent's display-name text as a second rank filter can drop older
		// versions whose upstream row names differ, turning a family picker back
		// into a one-row card. Rank the viable rows without a free-text request,
		// then apply the verified GroupId boundary below.
		request.Name = ""
	}
	if inferred, ok := currentTurnImageCatalogRequest(wfCtx); ok {
		// The catalog fact is the grounded interpretation of this turn. Do not also
		// score the Agent's free-text ImageName (for example "最新pytorch"): a row
		// whose display name happens to contain "pytorch" would receive an extra
		// vote and could outrank a newer runtime-named row from the same framework.
		// When the user chose the OTHER source, inferred.Name is deliberately the
		// frozen catalog phrase (for example "ComfyUI") and must remain: the other
		// source does not inherit the initial catalog's SoftwareFacts/Tags.
		request.Name = inferred.Name
		request.Framework = inferred.Framework
		request.Tag = inferred.Tag
		request.Source = inferred.Source
	}
	set := buildImageCandidateSetForRequest(
		wfCtx.Params, images, createImageTaxonomy(wfCtx), request,
	)
	if hasRecommendedCommunityScope {
		return scopeImageCandidateSet(set, scope)
	}
	return set
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

// workflowZoneEntry is THE single zone read: every zone consumer (create
// validation, net-optimizer, form labels, pod meta, zone_id args) resolves a zone
// through here, so the snapshot-vs-map decision lives in ONE place instead of a
// re-implemented branch at each site.
//
// The turn's zone catalog snapshot is the sole authority. It must be present and
// available: a missing/unavailable snapshot, or a zone it does not carry, is a hard
// failure — the create refuses rather than guessing a placement. (Before the zone
// convergence a nil snapshot fell back to per-zone param maps for unmigrated tests;
// those maps are gone, and Available() is nil-safe so a nil snapshot simply reports
// unavailable.)
func workflowZoneEntry(wfCtx *Context, zone string) (deployment.ZoneCatalogEntry, error) {
	cat := wfCtx.ZoneCatalog()
	if !cat.Available() {
		return deployment.ZoneCatalogEntry{}, fmt.Errorf("可用区目录当前不可用，无法安全创建，请稍后重试")
	}
	entry, ok := cat.Entry(zone)
	if !ok {
		return deployment.ZoneCatalogEntry{}, fmt.Errorf("可用区 %s 不在当前可用区目录中，无法安全创建", zone)
	}
	return entry, nil
}

// workflowZoneIDIndex maps numeric zone id → zone id string for the turn's zones,
// so a numeric-keyed payload (the GPU inventory) can be decoded to zone names, from
// the single authoritative snapshot. An absent/unavailable snapshot yields an empty
// index (nil-safe), never a per-zone param map.
func workflowZoneIDIndex(wfCtx *Context) map[uint32]string {
	out := map[uint32]string{}
	cat := wfCtx.ZoneCatalog()
	if !cat.Available() {
		return out
	}
	for _, zone := range cat.Zones() {
		if p, ok := cat.Placement(zone); ok && p.ZoneID != 0 {
			out[p.ZoneID] = zone
		}
	}
	return out
}

// workflowZonePlacement resolves a zone to its full placement — one record whose
// ZoneID/Region/AzGroup/IsPod cannot disagree — through the single workflowZoneEntry.
func workflowZonePlacement(wfCtx *Context, zone string) (deployment.ZonePlacement, error) {
	entry, err := workflowZoneEntry(wfCtx, zone)
	if err != nil {
		return deployment.ZonePlacement{}, err
	}
	return entry.Placement, nil
}

func validateCreatePlacement(wfCtx *Context, placement deployment.ZonePlacement, purchase bool) error {
	chargeType := createChargeType(wfCtx.Params)
	if placement.IsPod && placement.ZoneID == 0 {
		return fmt.Errorf("未获取到 %s 的内部可用区编号，无法安全创建。请稍后重试或到控制台确认可用区", zoneDisplayLabel(wfCtx, placement.Zone))
	}
	if placement.IsPod && purchase && placement.AzGroup == 0 {
		return fmt.Errorf("未获取到 %s 的内部地域编号，无法安全创建。请稍后重试或到控制台确认可用区", zoneDisplayLabel(wfCtx, placement.Zone))
	}
	if !purchase {
		return nil
	}
	pool := createInventoryPool(chargeType)
	// Only a KNOWN unsupported mode refuses. An unknown one does not: the same
	// rule that stops a zero inventory count from becoming "sold out" applies
	// here symmetrically — a missing observation is not a negative observation,
	// and refusing on it would turn one flaky read of a supplementary API into a
	// blocked create. The create API stays the authority for what it will accept.
	if supported, known := createInventoryPoolSupport(wfCtx, placement, pool); known && !supported {
		return fmt.Errorf("%s 的 %s 当前不支持%s购买方式，请选择其他计费方式或可用区",
			zoneDisplayLabel(wfCtx, placement.Zone), paramStr(wfCtx.Params, "GpuType", "该机型"), createInventoryPoolLabel(pool))
	}
	return nil
}

func createInventoryPool(chargeType string) string {
	if strings.EqualFold(chargeType, deployment.ChargeTypeSpot) {
		return deployment.GPUInventoryPoolSpot
	}
	return deployment.GPUInventoryPoolExclusive
}

func createInventoryPoolLabel(pool string) string {
	if pool == deployment.GPUInventoryPoolSpot {
		return "抢占式"
	}
	return "独占"
}

// createInventoryPoolSupport is the SINGLE purchase-mode fact for this workflow.
// Both create flows run the inventory steps, so the guided cards, the plain
// confirm card and the authoritative create gate all read this one answer —
// which is the whole point: a card must never offer a mode the gate will refuse,
// nor hide one the gate would allow.
//
// A missing snapshot is deliberately NOT read as "supported". It used to be, on
// the premise that the catalog query was already charge-type scoped; it is not
// (see stepQueryInstanceTypes for the measurement), so that premise silently
// disabled the gate on the plain path.
func createInventoryPoolSupport(wfCtx *Context, placement deployment.ZonePlacement, pool string) (bool, bool) {
	return createInventoryPoolSupportFor(wfCtx, placement, paramStr(wfCtx.Params, "GpuType", ""), pool)
}

// createInventoryPoolSupportFor is the same fact for a model the caller names
// explicitly. The GPU card needs it: it is deciding BETWEEN models, so the one
// in Params is not yet the one being judged.
func createInventoryPoolSupportFor(wfCtx *Context, placement deployment.ZonePlacement, gpuType, pool string) (bool, bool) {
	supported, known := deployment.InventoryPoolSupportFromResult(
		wfCtx.Result(createGPUInventoryStep), placement, gpuType, pool,
	)
	if known {
		return supported, true
	}
	// The official product contract always offers Postpay/Day/Month. Spot and
	// Pod pool membership depend on the live inventory metadata and must not be
	// guessed when that metadata is unavailable.
	if !placement.IsPod && pool == deployment.GPUInventoryPoolExclusive {
		return true, true
	}
	return false, false
}

func validateSelectedImageCompatibility(wfCtx *Context, imageID string, placement deployment.ZonePlacement) error {
	images := createImageResult(wfCtx)
	image := imageMapByID(images, imageID)
	name := imageNameByID(images, imageID)
	if name == "" {
		name = "所选镜像"
	}
	// The container/VM verdict comes from imageZoneCompatibility — the same call
	// the zone card makes — so the two cannot drift into different rules. This gate
	// is stricter by design on one verdict only: Unverifiable refuses here and does
	// not disable there, because refusing on missing evidence is the gate's job.
	switch imageContainerFitForZone(images, imageID, placement) {
	case imageContainerFitUnverifiable:
		return fmt.Errorf("未能确认 %s 是可用于 %s 的容器镜像，请刷新后重新选择", name, zoneDisplayLabel(wfCtx, placement.Zone))
	case imageContainerFitNeedsContainerImage:
		return fmt.Errorf("%s 不是容器镜像，不能用于 %s，请更换镜像或可用区", name, zoneDisplayLabel(wfCtx, placement.Zone))
	}
	if image == nil {
		// Community image searches can return a different page/order on a second
		// query. Keep the exact selected id for normal zones; the upstream capacity
		// preflight validates that id, its status, and its adaptive UHost image.
		return nil
	}
	if status := strings.TrimSpace(paramStr(image, "Status", "")); status != "" && !strings.EqualFold(status, deployment.ImageStatusAvailable) {
		return fmt.Errorf("%s 当前不可用，请更换镜像", name)
	}
	gpuType := paramStr(wfCtx.Params, "GpuType", "")
	supported := imageSupportedByID(images, imageID)
	if gpuType == "" || len(supported) == 0 || containsFold(supported, gpuType) {
		return nil
	}
	return fmt.Errorf("%s 不支持当前 GPU %s，请更换镜像或卡型", name, gpuType)
}

func workflowSystemDisks(wfCtx *Context, imageID, zone, gpuType string) []any {
	return deployment.ResolveBootDisk(createImageResult(wfCtx), wfCtx.Result("查询可用配比"), imageID, gpuType, zone)
}

func workflowMinimalCPUPlatform(wfCtx *Context, gpuType, zone string) string {
	if v := strings.TrimSpace(paramStr(wfCtx.Params, "MinimalCpuPlatform", "")); v != "" {
		if strings.EqualFold(v, deployment.MinimalCPUPlatformAuto) {
			if first := workflowFirstCPUPlatform(wfCtx.Result("查询可用配比"), gpuType, zone); first != "" {
				return first + "/Auto"
			}
		}
		return v
	}
	if first := workflowFirstCPUPlatform(wfCtx.Result("查询可用配比"), gpuType, zone); first != "" {
		return first + "/Auto"
	}
	return deployment.MinimalCPUPlatformAuto
}

func workflowFirstCPUPlatform(catalog map[string]any, gpuType, zone string) string {
	entry := workflowCatalogEntry(catalog, gpuType, zone)
	if entry == nil {
		return ""
	}
	raw, _ := entry["CpuPlatforms"].(map[string]any)
	if len(raw) == 0 {
		return ""
	}
	if _, ok := raw["Amd"]; ok {
		return "Amd"
	}
	if _, ok := raw["Intel"]; ok {
		return "Intel"
	}
	keys := make([]string, 0, len(raw))
	for key := range raw {
		if strings.TrimSpace(key) != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

func workflowCatalogEntry(catalog map[string]any, gpuType, zone string) map[string]any {
	if catalog == nil || gpuType == "" {
		return nil
	}
	types, _ := catalog["AvailableInstanceTypes"].([]any)
	var fallback map[string]any
	for _, item := range types {
		entry, _ := item.(map[string]any)
		if entry == nil {
			continue
		}
		if name, _ := entry["Name"].(string); name != gpuType {
			continue
		}
		if fallback == nil {
			fallback = entry
		}
		entryZone, _ := entry["Zone"].(string)
		if zone == "" || entryZone == "" || strings.EqualFold(entryZone, zone) {
			return entry
		}
	}
	return fallback
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

// priceAmountFor reads the amount quoted for one charge type out of one of the
// price arrays.
//
// It accepts both "Instance" and "Price". "Instance" is what upstream ACTUALLY
// returns — every live capture of GetCompShareInstancePriceResponse taken from
// the real API uses it, and "Price" appears in none of them. This function's doc
// used to say the opposite:
// that "Price" was the API field and "Instance" a robustness fallback. It was
// written to match this repo's fixtures, which invented "Price" — so the branch
// production has always taken was documented as the fallback, and the branch no
// live response can reach was documented as the contract. Both are read here
// because the fixtures still exist; the order is not a statement about upstream.
func priceAmountFor(raw map[string]any, arrKey, chargeType string) (float64, bool) {
	arr, ok := raw[arrKey].([]any)
	if !ok {
		return 0, false
	}
	for _, entry := range arr {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if ct, _ := m["ChargeType"].(string); ct != chargeType {
			continue
		}
		if n, ok := priceNumber(m["Instance"]); ok {
			return n, true
		}
		if n, ok := priceNumber(m["Price"]); ok {
			return n, true
		}
	}
	return 0, false
}

// priceListAmountFor reads the undiscounted price, which upstream reports under
// either name depending on the endpoint.
func priceListAmountFor(raw map[string]any, chargeType string) (float64, bool) {
	if list, ok := priceAmountFor(raw, "ListPriceDetails", chargeType); ok {
		return list, true
	}
	return priceAmountFor(raw, "OriginalPriceDetails", chargeType)
}

// estimatedPriceSuffix marks the quote as an estimate in the card's price VALUE,
// not only in a separate note field.
//
// The value is where it has to be. The HTTP confirmation frame hands these args to
// a frontend this repo does not own, which renders them with its own labels; a
// structured flag alone would be honest only once that frontend adopts it, and
// until then the user would read a bare number as a commitment. Upstream cannot
// hold a price, so the number is an estimate in every renderer or it is misleading
// in some of them.
const estimatedPriceSuffix = "（预估）"

// createPriceNote is the fuller sentence, carried as its own card field so a
// renderer can place it properly. CLI prints it under the price.
const createPriceNote = "最终费用以实际创建和结算结果为准"

// extractEstimatedPrice builds the snapshot of what the user is about to be
// quoted, and renders the one string that both the card and the seal will carry.
//
// It is the ONLY place a create price is turned into text. It absorbed
// confirmPriceText, which used to render it separately: that function re-ran the
// very same PriceDetails lookup this one had already done, so the "no price"
// guard here was dead — its own !ok branch could never be reached, because the
// second lookup's empty-string branch always caught the same case first. Two
// lookups of one response, one of them load-bearing and the other shadowing a
// guard, is the shape of defect this convergence exists to remove; a mutation test
// found it here rather than a user.
//
// Returns nil when upstream quoted nothing usable for this charge type — the card
// then shows no price rather than a fabricated one, because a 0 renders as free.
//
// It records only what upstream said. No quote id (there is none — SourceRequestID
// is the response's request_uuid and is named for what it is), no validity, no
// currency, and Locked=false because the platform cannot hold this number.
func extractEstimatedPrice(priceResult any, chargeType string) *EstimatedPriceSnapshot {
	raw, ok := priceResult.(map[string]any)
	if !ok {
		return nil
	}
	payable, ok := priceAmountFor(raw, "PriceDetails", chargeType)
	if !ok {
		return nil
	}
	text := fmt.Sprintf("¥%.2f%s", payable, chargePeriodUnit(chargeType))
	snapshot := &EstimatedPriceSnapshot{
		ChargeType:      chargeType,
		PayableAmount:   payable,
		SourceRequestID: paramStr(raw, "request_uuid", ""),
		Locked:          false,
	}
	if list, hasList := priceListAmountFor(raw, chargeType); hasList {
		snapshot.ListAmount = &list
		if list > payable {
			text += fmt.Sprintf("（原价 ¥%.2f）", list)
		}
	}
	snapshot.DisplayText = text + estimatedPriceSuffix
	return snapshot
}

// chargePeriodUnit maps a ChargeType to its billing-period suffix for display.
func chargePeriodUnit(chargeType string) string {
	switch chargeType {
	case "Day":
		return "/天"
	case "Month":
		return "/月"
	default: // Postpay / Spot are pay-as-you-go hourly
		return "/小时"
	}
}

// priceNumber coerces a JSON-decoded numeric price field to float64.
func priceNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	}
	return 0, false
}

func stepConfirmCreate() Step {
	return Step{
		Name: "确认创建",
		Type: StepConfirm,
		// Editable selection form (v1, select-only). Consumed only when the
		// HTTP path wires a ConfirmEditsFunc (COMPSHARE_CONFIRM_FORM on +
		// client opt-in); the CLI boolean confirm and the deploy_model saga
		// ignore these three fields entirely.
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

// materializeCreateDraft resolves every derived create parameter ONCE — zone,
// CPU/memory, card count, image id, charge type, minimal CPU platform, system
// disks, placement — and RETURNS the draft. It stores nothing; see the note at the
// end of this comment.
//
// This is the "form the CreateExecutionDraft" stage. Before it existed,
// resolveTargetSpec ran TWICE: once in buildCreateConfirmArgs to render the card,
// and again inside stepCreateInstance.BuildArgs to build the real API call — with
// the seal in between, hashing only Params and therefore blind to both. The card
// and the executed request agreed only because the function was pure and its
// inputs happened to be frozen after the gate (the old re-validation named only
// the stock and price steps, so "查询可用配比" never re-ran). That was an accident
// of the call graph, not a contract, and it covered Zone, CPU, Memory, GPU count,
// ImageId, ChargeType, MinimalCpuPlatform, disks and placement alike.
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
			Disks:              workflowSystemDisks(wfCtx, imageId, zone, gt),
			Name:               paramStr(wfCtx.Params, "Name", ""),
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
	// The SAME snapshot the card rendered — read once more, not rebuilt. Both the
	// decode above and the encode below copy the disks, so what lands in Params is
	// independent of StepResults all the way down.
	//
	// This comment used to claim the separation followed from ToContractMap
	// building a fresh map every call. It does not, and did not: the map was fresh
	// but the disk list inside it was the candidate's own, so the two were joined
	// at the one field that lives behind a reference. The codec now copies it; the
	// freshness of the map was never the thing doing the work.
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
	// every path today, so nothing was visibly broken — but "the card reads only
	// the draft" is either structural or it is a habit, and a habit is what the
	// card/create image split already turned out to be.
	// No price, no card. A create that reached this gate without a quote used to
	// render a card with the 价格 row silently missing on the CLI and "price":""
	// on the wire, and then let the user approve a spend nobody had priced. That
	// is the one thing docs/workflow-tool-retcode-audit.md refuses: "任何涉及费用
	// 的操作，要么在确认前展示有效价格，要么在确认前停止" — a rule ResizeInstanceWorkflow
	// and CFS create already honour through this same message, and which the
	// create was alone in not honouring.
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
		return nil, fmt.Errorf(missingWorkflowPriceMessage)
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
		// FallbackNote is set by the deploy_model handler when it switched the
		// create-zone (sold-out primary). Empty for the CLI/ReAct create path.
		// Surfaced in the confirm card so the user sees the zone switch before
		// approving. The key is always present (value "" when unset); the
		// renderer (cli.go printCreateConfirmCard) skips it when empty.
		"FallbackNote": paramStr(wfCtx.Params, "FallbackNote", ""),
	}
	if name := strings.TrimSpace(draft.Args.Name); name != "" {
		summary["Name"] = name
	}
	return summary, nil
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

func stepDescribeInstance() Step {
	return Step{
		Name:     "查看状态",
		Type:     StepToolCall,
		Tool:     "DescribeCompShareInstance",
		Optional: true,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			createResult := wfCtx.Result("创建实例")
			ids, ok := createResult["UHostIds"].([]any)
			if !ok || len(ids) == 0 {
				return nil, fmt.Errorf("创建实例未返回 UHostIds")
			}
			return map[string]any{
				"UHostIds": ids,
			}, nil
		},
	}
}

func createInstanceResultData(wfCtx *Context) map[string]any {
	createResult := wfCtx.Result("创建实例")
	if createResult == nil {
		return nil
	}
	if ids, ok := createResult["UHostIds"]; ok {
		return map[string]any{"UHostIds": ids}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

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
// sized the GPU for, instead of re-resolving independently). CLI/ReAct callers do
// NOT set it, so their resolution is byte-unchanged.
// SelectedImage is one image, chosen once: the ID that executes and the Name the
// user is shown are two fields of a SINGLE selection.
//
// They used to be two independent walks of the same response. For platform images
// pickImageId and pickImageName each called matchPlatformImage and read a
// different field off the result — agreeing because they shared a starting point,
// not because anything held them together. For community images they did not even
// read the same LEVEL: the id came from groups[0].Data[0], the display name from
// groups[0].ImageName. And when a caller threaded a CompShareImageId, the name
// shown was whatever ImageName came with it — which is exactly where a stale name
// can be displayed over a different image's id.
type SelectedImage struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Source string `json:"source"`
}

// selectCreateImage resolves the image ONCE, through the single deterministic
// interpreter (deployment.ResolveImage) on the turn's image catalog. It is the only
// image decision in the create flow; the draft carries the result whole, so the
// confirm card renders Name and the create sends ID without either re-selecting.
//
// It replaces the old matchPlatformImage / selectCommunityImage / catalogImageName
// trio and the two invariants they violated:
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
	source := paramStr(params, "ImageSource", "platform")
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
		// Community FuzzySearch narrows upstream; the platform query no longer does.
		Prefiltered: source == "community" && strings.TrimSpace(paramStr(params, "ImageName", "")) != "",
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
	tag := source
	if tag == "" || tag == "community" {
		tag = "platform"
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
		if paramStr(wfCtx.Params, "ImageSource", "platform") == "community" {
			return deployment.NewImageCatalogSnapshot(true, deployment.ParseCommunityImageEntries(result))
		}
		return deployment.NewImageCatalogSnapshot(true, deployment.ParsePlatformImageEntries(result, "platform"))
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

// ---------------------------------------------------------------------------
// Editable confirm form (v1, select-only)
//
// All option sets are assembled from data ALREADY collected by earlier steps
// (查询镜像 / 查询可用配比) — zero extra API calls, zero LLM. No stock or price
// claim is made for combinations that were never checked: after an override
// the 检查库存/查询价格 steps re-run and a refreshed card is re-confirmed
// (方案 A), so the authoritative answer always precedes creation.
// ---------------------------------------------------------------------------

const (
	maxFormGPUOptions     = 5
	maxFormImageOptions   = 3
	maxGuidedImageOptions = 10
	// maxGuidedCommunityImageQueryLimit sizes the community browse corpus.
	//
	// 20 groups could not populate a 用途 classification: measured live 2026-07-22 the
	// community catalog holds TotalCount=821 groups, and one page of 100 already
	// spans all 7 categories (219 version rows, 219 of them tagged, 37 distinct
	// tags). Upstream tag filtering cannot narrow this for us — DescribeCommunityImages
	// declares Tag []string but its task never parses the dotted Tag.N form the way
	// DescribeAvailableCompShareInstanceTypes does for MachineTypes, so every Tag
	// value (valid, category name, or garbage) returns the identical unfiltered
	// result. Categorisation is therefore client-side over whatever this fetch
	// returns, which is why the fetch has to be worth categorising.
	//
	// It costs no model tokens: workflow step results stay in wfCtx and only
	// ResultData (UHostIds) leaves the workflow.
	//
	// ⚠️ The SortCondition sent with this query does NOT reach the ordering.
	// Measured live 2026-07-28 against the full catalog (832 groups, fetched by
	// paging): page 1 of 100 contains only 8 of the catalog's 20 most-deployed
	// families — RVC (#2, 9683 deploys), vLLM-DeepSeek-R1-Distill (#6) and
	// GPT-SoVITS (#10) are all absent — and all four argument shapes (plain,
	// SortCondition, ExcludeReadme, both) return byte-identical coverage. So this
	// is an ARBITRARY 100 of 832 with respect to popularity, not the top 100. The
	// picker still works (it classifies and ranks whatever it gets), but nothing
	// here may claim the browse corpus is popularity-ordered, and a "most popular"
	// answer cannot be derived from it without fetching every page.
	maxGuidedCommunityImageQueryLimit = 100
	// maxPlatformImageQueryLimit asks for the whole platform catalog in one call.
	//
	// The previous value of 20 was not a page size the flow ever paged past: no
	// Offset is sent, so whatever the first response held WAS the catalog for every
	// downstream consumer — the facet options, the picker and the final card.
	//
	// Measured live 2026-07-22 (TotalCount=72 throughout):
	//
	//	Limit absent / 20   rows=40  rows carrying tags=7
	//	Limit=100           rows=72  rows carrying tags=36
	//	Limit=200           upstream RetCode=230 "Params [Limit] not available"
	//
	// So 20 hid 44% of the catalog and 80% of the tagged rows, which is why the
	// tag facet looked nearly empty. 100 is also the practical ceiling — 200 is
	// refused — so do NOT raise this further without re-probing. Should the catalog
	// ever exceed 100, TotalCount in the response is the signal that the list is
	// truncated; today 72 < 100 so it is complete.
	maxPlatformImageQueryLimit = 100
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
// This used to answer "is it a pod zone", which is the rule the create gate
// itself dropped: pod-ness does not decide Spot. 华北二C is a pod zone that DOES
// sell Spot, and 华北一C is a pod zone that does not — a card built on pod-ness
// hides the first and the gate would have accepted it. Reading the same fact the
// gate reads is the only way the two cannot disagree.
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
// Within its one axis it is the single implementation, consumed by BOTH the zone
// card and the create gate. They used to compute it separately from the same two
// inputs — the same answer today, and a drift waiting to happen the first time
// either side is touched, which is precisely how a card comes to offer what the
// gate refuses.
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
// zone only on a KNOWN mismatch. Before this gate existed the pair was assembled
// silently — the flow's own default zone could pick the incompatible one — and
// surfaced as "Ubuntu-nvidia 22.04 不是容器镜像，不能用于 上海二A" after the last
// card, when the only remedy left was to start over.
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
	if strings.EqualFold(strings.TrimSpace(paramStr(params, "ImageSource", "")), "community") {
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
	// monotonic ordinal and Total=0 (unknown) instead of renumbering history from
	// mutable skip predicates (the old 3/9 -> 3/8 and 6/6 -> 5/5 bug).
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

// markGuidedStepReached records that a card was shown, in the order the cards
// were reached — which is what the step ordinal is derived from.
//
// It used to also maintain a GuidedReachedSteps set for a membership query. The
// final card stopped asking whether the image step had been reached (it no
// longer re-opens the image at all), which left that set written and never read
// — dead state inside Params, and Params is what seal() hashes.
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

func guidedStepSkipped(wfCtx *Context, logical int) bool {
	var (
		skip bool
		err  error
	)
	switch logical {
	case guidedStepGPU:
		skip, err = shouldSkipGuidedGPUStep(wfCtx)
	case guidedStepZone:
		skip, err = shouldSkipGuidedZoneStep(wfCtx)
	case guidedStepGPUCount:
		skip, err = shouldSkipGuidedGPUCountStep(wfCtx)
	case guidedStepCPUMemory:
		skip, err = shouldSkipGuidedCPUMemoryStep(wfCtx)
	case guidedStepImageSource:
		skip, err = shouldSkipGuidedImageSourceStep(wfCtx)
	case guidedStepImageFacets:
		skip, err = shouldSkipGuidedImageFacetsStep(wfCtx)
	case guidedStepImageTag:
		skip, err = shouldSkipGuidedImageTagStep(wfCtx)
	case guidedStepImageFamily:
		skip, err = shouldSkipGuidedImageFamilyStep(wfCtx)
	case guidedStepImage:
		skip, err = shouldSkipGuidedImageStep(wfCtx)
	case guidedStepChargeType:
		skip, err = shouldSkipGuidedChargeTypeStep(wfCtx)
	case guidedStepFinal:
		return false
	default:
		return false
	}
	return err == nil && skip
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
	return strings.EqualFold(selected, current) && enabledOptionExists(opts, current), nil
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
	return selected == current && enabledOptionExists(opts, value), nil
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
	return selected == current && enabledOptionExists(opts, current), nil
}

// shouldSkipGuidedImageSourceStep and shouldSkipGuidedImageFacetsStep gate the two-stage
// image flow on imageUserSettled (the USER settled a concrete image), NOT on
// hasExplicitImageIntent — so BOTH steps show for community BROWSING (source chosen, no
// concrete image yet), which is exactly the case the two-stage flow serves. An Agent
// SUGGESTION is not settlement, so these steps show for it too — the user still chooses.
//
// A name alone no longer settles SOURCE: ComfyUI/SD-WebUI can exist in both live
// catalogs. Source is skipped only when the user explicitly chose it, a concrete
// verified id already owns it, a prior recommendation supplied id+source, or the
// alternate live-catalog probe proved the current source is the only match. The
// concrete image picker remains independently confirm-gated in every case.
func shouldSkipGuidedImageSourceStep(wfCtx *Context) (bool, error) {
	if wfCtx == nil {
		return false, nil
	}
	concreteUserImage := imageUserSettled(wfCtx) &&
		strings.TrimSpace(paramStr(wfCtx.Params, "CompShareImageId", "")) != ""
	return wfCtx.ImageSourceUserPinned() ||
		guidedStepWasReached(wfCtx, guidedStepImageSource) ||
		concreteUserImage ||
		imageSuggestionSettlesAxis(wfCtx) ||
		catalogIntentUniquelySettlesCurrentSource(wfCtx), nil
}

// currentTurnImageCatalogRequest is the deterministic fallback for an Agent
// proposal that omitted ImageName or supplied only a free-text suggestion even
// though the current user turn named a live catalog fact.
//
// It is intentionally narrow:
//   - a concrete id keeps its ordinary path;
//   - the frozen initial request is structured and literal; if the user chooses
//     the other source, only its same catalog-derived phrase is carried across;
//   - a user-pinned specific name stays a name and is never broadened to a
//     framework merely because one word overlaps.
//
// The returned request only ranks the picker. It never writes Params, never marks
// the image user-settled, and never skips the concrete-image confirmation.
func currentTurnImageCatalogRequest(wfCtx *Context) (deployment.ImageRequest, bool) {
	if wfCtx == nil ||
		strings.TrimSpace(paramStr(wfCtx.Params, "CompShareImageId", "")) != "" {
		return deployment.ImageRequest{}, false
	}
	seed, ok := currentImageCatalogIntentSeed(wfCtx)
	if !ok {
		return deployment.ImageRequest{}, false
	}
	source := normalizedImageSource(paramStr(wfCtx.Params, "ImageSource", "platform"))
	request := seed.Request
	if source == seed.InitialSource && strings.TrimSpace(request.Name) != "" {
		// A name already present in Params has the ordinary name-guided picker
		// path. The seed records it only so the opposite source can be checked and,
		// if the user switches, the same ask survives clearing source-local fields.
		return deployment.ImageRequest{}, false
	}
	if source != seed.InitialSource {
		// SoftwareFacts/Tags are source-local evidence. Carry only the phrase to
		// the other catalog and let its real display/family names establish the
		// candidates; do not pretend community rows inherited platform metadata.
		request = deployment.ImageRequest{Name: seed.Query}
	}
	request.Source = source
	if len(deployment.RankImages(createImageCatalog(wfCtx), request)) == 0 {
		return deployment.ImageRequest{}, false
	}
	return request, true
}

// imageSuggestionSettlesAxis reports that the Agent's suggestion has already
// answered the SOURCE and 用途 questions, so re-asking them is a question whose
// answer is visible in the suggestion itself.
//
// The distinction this draws is between the axes an image implies and the image
// itself. After 「推荐一个做数字人的镜像」→「用该镜像开一台」, asking 平台还是社区 and
// then 想跑哪一类 makes the user re-derive facts already fixed by the image the
// assistant named — measured on the real stack 2026-07-29, that flow re-opened
// at step 1 of the guided create. The picker still shows (it stays gated on
// imageUserSettled), so the guarantee that closed the original bug — an
// Agent-pinned id never seals unseen — is untouched: the user still sees and
// confirms the image, just without two cards that could only be answered one way.
//
// It requires the proposal to have NAMED the source rather than reading the
// defaulted value: paramStr falls back to "platform", so a community suggestion
// whose source the Agent left out would otherwise skip the source card while
// carrying the wrong catalog into the picker. initialParamSet is the same
// "the request actually said this" test the spec cards use.
func imageSuggestionSettlesAxis(wfCtx *Context) bool {
	if wfCtx == nil || wfCtx.ImageSelection() != ImageSelectionSuggested {
		return false
	}
	if strings.TrimSpace(paramStr(wfCtx.Params, "CompShareImageId", "")) == "" {
		return false
	}
	return initialParamSet(wfCtx, "ImageSource")
}

func shouldSkipGuidedImageFacetsStep(wfCtx *Context) (bool, error) {
	if _, catalogIntent := currentTurnImageCatalogRequest(wfCtx); imageUserSettled(wfCtx) ||
		imageSuggestionSettlesAxis(wfCtx) || catalogIntent {
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
	if _, catalogIntent := currentTurnImageCatalogRequest(wfCtx); imageUserSettled(wfCtx) ||
		imageSuggestionSettlesAxis(wfCtx) || catalogIntent {
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
	if imageUserSettled(wfCtx) || imageSuggestionSettlesAxis(wfCtx) {
		return true, nil
	}
	if _, catalogIntent := currentTurnImageCatalogRequest(wfCtx); catalogIntent {
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
	// The picker RESOLVES a concrete image, so skip it only when one is already
	// settled and needs no resolution: the user settled the image (imageUserSettled —
	// their text pinned it, or they picked on the card, which also sets a concrete id)
	// AND a concrete id exists. A bare user NAME is not a concrete id, so the picker
	// still runs (ranked, preselected). An Agent SUGGESTION is not settlement
	// (imageUserSettled is false for it), so the picker runs preselected on the
	// suggestion rather than sealing it unseen — an Agent-pinned id skipping this card
	// entirely (CompShareImageId != "" alone meant "settled") was the bug this closes.
	if !imageUserSettled(wfCtx) {
		return false, nil
	}
	return strings.TrimSpace(paramStr(wfCtx.Params, "CompShareImageId", "")) != "", nil
}

// imageUserSettled reports positive user authorization instead of inferring it
// from a non-empty id/name. Only engine-verified user provenance or an explicit
// picker submission settles the image; Agent-supplied values remain suggestions.
func imageUserSettled(wfCtx *Context) bool {
	if wfCtx == nil || !hasExplicitImageSelection(wfCtx.Params) {
		return false
	}
	if paramBool(wfCtx.Params, "GuidedImageLocked", false) {
		return true
	}
	return wfCtx.ImageSelection() == ImageSelectionUserPinned
}

// shouldSkipSourceReQuery skips the post-source re-query when an explicit image is
// pinned (no browsing) or when the guided source step did NOT change the source from the
// initial one — then the first 查询镜像 already fetched the right source and its result
// is authoritative. When the source DID change (either direction — platform↔community),
// the re-query replaces the stale initial catalog with the chosen source's, so the
// facets/picker/resolve steps never read a foreign-source listing.
func shouldSkipSourceReQuery(wfCtx *Context) (bool, error) {
	if imageUserSettled(wfCtx) &&
		strings.TrimSpace(paramStr(wfCtx.Params, "CompShareImageId", "")) != "" {
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

func shouldSkipGuidedChargeTypeStep(wfCtx *Context) (bool, error) {
	// The USER already said it ("用抢占式创建一台…"), so asking again is asking them to
	// repeat themselves. Same rule the GPU and zone cards use for an explicit value.
	//
	// This reads the provenance-derived flag, not the presence of ChargeType in
	// Params. The two are not the same question: the create tool's schema says
	// "默认 Postpay", so the Agent fills the field in on requests that never
	// mentioned billing, and key-presence therefore skipped the card for precisely
	// the users who had chosen nothing. See ReferenceData.ChargeTypeUserPinned.
	if wfCtx != nil && wfCtx.referenceData.ChargeTypeUserPinned {
		return true, nil
	}
	// Nothing to choose between: one selectable mode is an answer, not a question.
	selectable := 0
	for _, opt := range guidedChargeTypeOptions(wfCtx) {
		if !opt.Disabled {
			selectable++
		}
	}
	if selectable <= 1 {
		return true, nil
	}
	// Don't turn a card-free flow into a one-card flow. A request that pinned
	// everything else goes straight to the confirmation today; adding a question
	// there would interrogate the one user who asked for nothing. The charge type
	// keeps its default and the final card states it.
	return guidedChargeTypeIsTheOnlyCard(wfCtx), nil
}

// chargeTypeChangeHint says where the purchase mode can be changed. The final
// card deliberately cannot change it — a late switch would desync the resource
// pool every earlier step queried against — so the honest answer depends on
// whether this run actually showed the purchase-mode card. It did: point back at
// it. It did not (the user named the mode in their request): starting over is
// genuinely the only way.
//
// This sentence was a constant that always said "重新发起创建", written when the
// card did not exist. Once it did, the final card was telling users to redo the
// whole request to change something they had just been asked.
// The alternatives are computed by SUBTRACTING the mode in force, not listed as
// a constant. The constant version named all three of 包日/包月/抢占式 whatever the
// user had asked for, so a 抢占式 request produced "需要包日、包月或抢占式，请重新发起
// 创建（例如「用抢占式创建一台…」）" — it offered the mode already in force as the way
// to change away from it, and the example was verbatim what the user had just
// typed. The user is left unable to tell whether the card understood them.
func chargeTypeChangeHint(wfCtx *Context) string {
	if !guidedStepSkipped(wfCtx, guidedStepChargeType) {
		return "需要改用其他计费方式，请返回上面的「购买方式」一步重新选择。"
	}
	others := chargeTypeAlternativeLabels(createChargeType(wfCtx.Params))
	example, ok := chargeTypeAlternativeExample(createChargeType(wfCtx.Params))
	if len(others) == 0 || !ok {
		return "需要改用其他计费方式，请重新发起创建并直接说明。"
	}
	return fmt.Sprintf("需要改用%s，请重新发起创建并直接说明（例如「用%s创建一台…」）。",
		strings.Join(others, "、"), example)
}

// chargeTypeAlternativeLabels names the purchase modes OTHER than the one in
// force, reusing the option labels so the card cannot call a mode something the
// purchase-mode card does not.
func chargeTypeAlternativeLabels(current string) []string {
	out := make([]string, 0, len(createFormChargeTypes))
	for _, opt := range createFormChargeTypes {
		if strings.EqualFold(opt.Value, current) {
			continue
		}
		out = append(out, opt.Label)
	}
	return out
}

// chargeTypeAlternativeExample picks the phrase for the "例如「用X创建一台…」" hint.
// It comes from deployment's parsing vocabulary rather than from the display
// label: the label may carry a parenthetical ("按量付费（按小时计费）") that reads
// wrong inside a quoted sentence and, more importantly, the point of the example
// is that retyping it works — so it has to be a phrase the server resolves.
func chargeTypeAlternativeExample(current string) (string, bool) {
	for _, opt := range createFormChargeTypes {
		if strings.EqualFold(opt.Value, current) {
			continue
		}
		if phrase, ok := deployment.ExplicitChargeTypePhrase(opt.Value); ok {
			return phrase, true
		}
	}
	return "", false
}

func guidedChargeTypeIsTheOnlyCard(wfCtx *Context) bool {
	for step := guidedStepFirst; step < guidedStepFinal; step++ {
		if step == guidedStepChargeType {
			continue
		}
		if !guidedStepSkipped(wfCtx, step) {
			return false
		}
	}
	return true
}

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
	if selected == "" {
		return nil, fmt.Errorf("暂无有库存的 GPU 型号，请换一个型号或稍后再试")
	}
	index, total := guidedStepPosition(wfCtx, guidedStepGPU)
	title := guidedStepTitle(index, "请选择 GPU 参数")
	desc := "GPU 型号决定可用显存与算力：显存越大，越能支撑更大的模型与更高的批量。不确定时可先用默认项。"
	if locked {
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

// imageSourceFacetOptions lists the image sources the create flow really supports.
// custom/shared are deliberately absent — create forbids them (the tool schema enum
// is platform/community), so the facet never offers a source that would be rejected.
// imageSourceFacetOptions asks what the user wants to DO, and answers with which
// catalog to read.
//
// The two values stay platform/community because that is what they select: the
// catalogs live behind different endpoints with different response shapes
// (ImageSet[] vs CompshareImageGroup[].Data[]), so this is genuinely a choice of
// where to look, not a field that could be enumerated from one catalog. What
// changed is the QUESTION. "平台镜像 / 社区镜像" asks the user to know how this
// platform files its images before they can say what they want to run.
//
// The framing is not decoration — it is what the two catalogs actually are.
// Measured live 2026-07-22: community is 821 groups, 219/219 of the fetched rows
// tagged, and those tags ARE the platform's 用途 classification — a catalog of
// ready-to-run applications. Platform is 72 rows: 9 bare System images and 52 App
// images whose tags are framework names (PyTorch, Miniconda3, Tensorflow) — a
// catalog of environments to build in.
//
// Order and default are deliberately unchanged: platform stays first and stays the
// default, so this reframes the question without silently moving anyone to a
// different catalog.
func imageSourceFacetOptions() []ConfirmFormOption {
	return []ConfirmFormOption{
		{
			Value: "platform", Label: "平台镜像",
			Note: "平台官方镜像：干净的系统镜像，或预装 PyTorch / TensorFlow 等框架的基础镜像",
		},
		{
			Value: "community", Label: "社区镜像",
			Note: "社区镜像：开箱即用的应用与模型，可按数字人、图像视频生成、语音、LLM 等用途挑选",
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
// The enum is System / App / Game / Other (DescribeCompShareImages), and the live
// platform catalog really does return all of System(9), App(52) and Other(11) —
// "Other" used to fall through and render as the bare English word next to three
// Chinese labels. An unrecognised type still falls through verbatim on purpose: a
// value we cannot name is shown as the platform sent it rather than guessed at.
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
// The raw ImageTag facet used to sit on this same card. It now has its own card
// (stepGuidedChooseImageTag) so its options can be computed from what THIS card's
// answer leaves behind — see guidedStepImageTag.
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
	source := paramStr(wfCtx.Params, "ImageSource", "platform")
	if source != "community" {
		source = "platform"
	}
	// When the Agent/default started in a catalog with no related row and the
	// successfully checked opposite catalog has matches, recommend that real
	// source on the card. This remains only the form value: Params changes after
	// the user submits, never from the probe itself.
	if !wfCtx.ImageSourceUserPinned() {
		if seed, ok := currentImageCatalogIntentSeed(wfCtx); ok && seed.InitialMatches == 0 {
			if matches, checked := alternateImageCatalogMatchCount(wfCtx, seed); checked && matches > 0 {
				source = oppositeImageSource(seed.InitialSource)
			}
		}
	}
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
// candidate list is longer than the page, what it is NOT showing.
//
// Silence here was the defect: the earlier filter card advertised "框架 / 应用镜像
// 55 个镜像" and this card handed back ten with no sign that forty-five existed.
// Neither available lie is acceptable — restating the count as ten hides real
// candidates, and listing all fifty-five makes the card unusable — so the card
// says both numbers and names the two ways to narrow. Real paging/search is still
// owed; this is the honest interim.
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
			// The charge type is stated, not offered. It is settled before step one
			// (see above) and there is no earlier card to point at, so saying "已在
			// 前面选定" would send the user looking for a control that does not
			// exist. Naming the current value and how to change it is the honest
			// version — the Summary carries it too, alongside the price it produced.
			Description: fmt.Sprintf(
				"镜像决定开机即用的预装环境（框架与驱动）。当前计费方式为「%s」，价格按此计算；%s确认无误后点击下方按钮即开始创建。",
				chargeTypeLabel(createChargeType(wfCtx.Params)),
				chargeTypeChangeHint(wfCtx)),
			PrimaryLabel:   "确认部署",
			SecondaryLabel: "取消",
			Final:          true,
		},
		Fields: fields,
	}, nil
}

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
	delete(wfCtx.Params, "CompShareImageId")
	delete(wfCtx.Params, "ImageName")
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

// normalizedImageSource collapses any input to the two sources create supports;
// anything that is not community is platform.
func normalizedImageSource(v string) string {
	if strings.EqualFold(strings.TrimSpace(v), "community") {
		return "community"
	}
	return "platform"
}

// hasExplicitImageSelection reports a concrete image already pinned by id or name —
// distinct from hasExplicitImageIntent, which ALSO treats ImageSource=community as
// "intent". The two-stage source/facets steps must show for community BROWSING (source
// chosen, no concrete image yet), so they gate on this narrower predicate.
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
	// The user picked on the picker: the image is now user-settled. Subsequent
	// re-runs (a later-card edit re-runs the flow) read this and skip the image
	// cards, instead of seeing state==Suggested and re-opening the picker as if the
	// pick were a fresh Agent suggestion. Cleared by the source/type/tag/GPU edits
	// that invalidate the pinned image, so a re-browse starts clean.
	wfCtx.Params["GuidedImageLocked"] = true
	markGuidedStepReached(wfCtx, guidedStepImage)
	return nil
}

func ensureGuidedGPUType(wfCtx *Context) (string, error) {
	current, _ := wfCtx.Params["GpuType"].(string)
	supported := currentImageSupportedGPUs(wfCtx.Params, createImageResult(wfCtx))
	locked := paramBool(wfCtx.Params, "GuidedGpuLocked", false) && current != ""
	selected, opts := guidedGPUFormOptions(wfCtx, wfCtx.Result("查询可用配比"), supported, current, locked, wfCtx.Params, wfCtx.Result("查询GPU库存"))
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

type guidedInventory struct {
	counts        map[string]map[string]map[string]float64
	preferredPool string
}

func guidedInventoryFrom(wfCtx *Context, result map[string]any) guidedInventory {
	zoneByID := workflowZoneIDIndex(wfCtx)
	if len(zoneByID) == 0 || result == nil {
		return guidedInventory{}
	}
	rawInv, _ := result["GpuInventory"].(map[string]any)
	if rawInv == nil {
		return guidedInventory{}
	}
	preferredPool := deployment.GPUInventoryPoolExclusive
	if strings.EqualFold(createChargeType(wfCtx.Params), "Spot") {
		preferredPool = deployment.GPUInventoryPoolSpot
	}
	counts := map[string]map[string]map[string]float64{}
	for _, poolName := range []string{deployment.GPUInventoryPoolExclusive, deployment.GPUInventoryPoolSpot} {
		rawPool, _ := rawInv[poolName].(map[string]any)
		for rawZoneID, rawGPUCounts := range rawPool {
			id, ok := parseUint32Any(rawZoneID)
			if !ok {
				continue
			}
			zone := zoneByID[id]
			if zone == "" {
				continue
			}
			gpuCounts, _ := rawGPUCounts.(map[string]any)
			if gpuCounts == nil {
				continue
			}
			if counts[zone] == nil {
				counts[zone] = map[string]map[string]float64{}
			}
			if counts[zone][poolName] == nil {
				counts[zone][poolName] = map[string]float64{}
			}
			for gpuType, rawCount := range gpuCounts {
				counts[zone][poolName][gpuType] = anyFloat(rawCount)
			}
		}
	}
	if len(counts) == 0 {
		return guidedInventory{}
	}
	return guidedInventory{counts: counts, preferredPool: preferredPool}
}

// addZoneRegionAndID stamps the read-probe query with the zone's Region and
// internal id taken from ONE catalog record, so the two fields can never come
// from different sources. On a present-but-unresolvable snapshot (unavailable,
// or the zone absent) it stamps nothing rather than string-guessing a Region for
// a zone the authority rejected — the create refuses downstream anyway.
func addZoneRegionAndID(wfCtx *Context, args map[string]any, zone string) map[string]any {
	entry, err := workflowZoneEntry(wfCtx, zone)
	if err != nil {
		return args
	}
	if r := strings.TrimSpace(entry.Placement.Region); r != "" {
		args["Region"] = r
	}
	if entry.Placement.ZoneID != 0 {
		args["zone_id"] = entry.Placement.ZoneID
	}
	return args
}

func syncGuidedZoneMeta(wfCtx *Context, zone string) {
	if wfCtx == nil || strings.TrimSpace(zone) == "" {
		return
	}
	// The pod flag is the snapshot record's. An unresolvable zone (unavailable
	// snapshot, or a zone the catalog does not carry) writes nothing.
	entry, err := workflowZoneEntry(wfCtx, zone)
	if err != nil {
		return
	}
	wfCtx.Params["ZoneIsPod"] = entry.Placement.IsPod
	wfCtx.Params["IsPodZone"] = entry.Placement.IsPod
}

func parseUint32Any(v any) (uint32, bool) {
	switch x := v.(type) {
	case uint32:
		return x, x != 0
	case uint64:
		if x == 0 || x > uint64(^uint32(0)) {
			return 0, false
		}
		return uint32(x), true
	case int:
		if x <= 0 {
			return 0, false
		}
		return uint32(x), true
	case int64:
		if x <= 0 || x > int64(^uint32(0)) {
			return 0, false
		}
		return uint32(x), true
	case float64:
		if x <= 0 || x > float64(^uint32(0)) || x != float64(uint32(x)) {
			return 0, false
		}
		return uint32(x), true
	case string:
		n, err := strconv.ParseUint(strings.TrimSpace(x), 10, 32)
		if err != nil || n == 0 {
			return 0, false
		}
		return uint32(n), true
	default:
		return 0, false
	}
}

func anyFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case uint32:
		return float64(x)
	case uint64:
		return float64(x)
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f
	default:
		return 0
	}
}

func (inv guidedInventory) count(zone, gpuType string) (float64, bool) {
	if inv.counts == nil || zone == "" || gpuType == "" {
		return 0, false
	}
	pools, ok := inv.counts[zone]
	if !ok {
		return 0, false
	}
	if gpus, ok := pools[inv.preferredPool]; ok {
		if count, present := gpus[gpuType]; present {
			return count, true
		}
	}
	// Pod zones may expose only Spot inventory even before the user reaches the
	// charge-type form. Use the other explicitly returned pool rather than
	// presenting the zone as unknown; this is still a real backend observation.
	for _, pool := range []string{deployment.GPUInventoryPoolExclusive, deployment.GPUInventoryPoolSpot} {
		if pool == inv.preferredPool {
			continue
		}
		if gpus, ok := pools[pool]; ok {
			if count, present := gpus[gpuType]; present {
				return count, true
			}
		}
	}
	return 0, false
}

func (inv guidedInventory) total(zones []string, gpuType string) (float64, bool) {
	var total float64
	known := false
	for _, zone := range zones {
		if count, ok := inv.count(zone, gpuType); ok {
			known = true
			total += count
		}
	}
	return total, known
}

// guidedStockNote renders the raw DescribeCompShareGpuInventory reading. That source is
// a SNAPSHOT and is not authoritative — real creatability comes from
// CheckCompShareResourceCapacity (which needs a GPU type + zone as input, so it cannot
// gate this earlier step) and finally from 检查库存 on the sealed config. The wording
// must therefore never read as a promise: a card that said a sold-out GPU was available
// is exactly the reported failure.
func guidedStockNote(count float64) string {
	if count <= 0 {
		return "库存快照为 0，待确认"
	}
	return fmt.Sprintf("库存快照约 %.0f 张 GPU，待确认", count)
}

func guidedStockFitNote(free, requested float64) string {
	if free >= requested {
		return "当前库存可满足"
	}
	if free <= 0 {
		return "库存快照为 0，待确认"
	}
	return fmt.Sprintf("库存快照仅剩 %.0f 张 GPU，待确认", free)
}

func firstEnabledValue(opts []ConfirmFormOption) string {
	for _, opt := range opts {
		if !opt.Disabled {
			return opt.Value
		}
	}
	return ""
}

func guidedGPUFormOptions(wfCtx *Context, catalog map[string]any, supported []string, current string, locked bool, params map[string]any, inventoryResult map[string]any) (string, []ConfirmFormOption) {
	if catalog == nil {
		return current, nil
	}
	inventory := guidedInventoryFrom(wfCtx, inventoryResult)
	candidateOrder, candidateSet := guidedCandidateGPUSet(params)
	if locked {
		candidateOrder, candidateSet = nil, nil
	}
	reasons := guidedGPUReasons(params)
	type gpuChoice struct {
		name    string
		normal  bool
		vramGB  float64
		zones   []string
		current bool
	}
	choices := map[string]*gpuChoice{}
	order := []string{}
	types, _ := catalog["AvailableInstanceTypes"].([]any)
	for _, t := range types {
		mt, _ := t.(map[string]any)
		name, _ := mt["Name"].(string)
		if name == "" {
			continue
		}
		if locked && current != "" && !guidedGPUIntentMatches(current, name) &&
			(len(supported) == 0 || !containsFold(supported, name)) {
			continue
		}
		if len(candidateSet) > 0 && !candidateSet[strings.ToLower(name)] && !strings.EqualFold(name, current) {
			continue
		}
		ch, ok := choices[name]
		if !ok {
			ch = &gpuChoice{name: name}
			choices[name] = ch
			order = append(order, name)
		}
		if name == current {
			ch.current = true
		}
		status, _ := mt["Status"].(string)
		if status == "" || strings.EqualFold(status, "Normal") {
			ch.normal = true
		}
		if z, _ := mt["Zone"].(string); z != "" && !containsFold(ch.zones, z) {
			ch.zones = append(ch.zones, z)
		}
		if gm, _ := mt["GraphicsMemory"].(map[string]any); gm != nil {
			if v, _ := gm["Value"].(float64); v > 0 && ch.vramGB == 0 {
				ch.vramGB = v
			}
		}
	}
	if current != "" {
		if _, ok := choices[current]; !ok {
			choices[current] = &gpuChoice{name: current, current: true}
			order = append([]string{current}, order...)
		}
	}
	if len(candidateOrder) > 0 {
		var filtered []string
		seen := map[string]bool{}
		for _, name := range candidateOrder {
			if choices[name] == nil || seen[strings.ToLower(name)] {
				continue
			}
			filtered = append(filtered, name)
			seen[strings.ToLower(name)] = true
		}
		order = filtered
	}
	// Real creatability for the exact image / disk / charge type this create will
	// send, per (model, zone). A combination the probe could not answer is absent
	// and reads as unknown — never as a refusal.
	combos := comboCreatability(wfCtx.Result(zoneCapacityStepName))
	// Same escape hatch the zone card has: the gate STEERS, it does not refuse.
	// If nothing is creatable there is nothing to steer toward, and graying out
	// every model leaves a card that offers nothing — ensureGuidedGPUType turns
	// that into a dead end raised before the draft exists, losing both the
	// candidate draft and the typed capacity_sold_out reason the sold-out reply is
	// built from. That authoritative negative belongs to 检查库存.
	anyModelCreatable := false
	for _, name := range order {
		if ch := choices[name]; ch != nil {
			if ok, known := gpuModelCreatable(combos, ch.name, ch.zones); ok && known {
				anyModelCreatable = true
				break
			}
		}
	}
	var opts []ConfirmFormOption
	appendChoice := func(name string) {
		ch := choices[name]
		if ch == nil {
			return
		}
		label := ch.name
		if ch.vramGB > 0 {
			label = fmt.Sprintf("%s（%.0fG显存）", ch.name, ch.vramGB)
		}
		noteParts := []string{}
		if reason := reasons[strings.ToLower(ch.name)]; reason != "" {
			noteParts = append(noteParts, reason)
		}
		if ch.vramGB > 0 {
			noteParts = append(noteParts, fmt.Sprintf("%.0fG 显存", ch.vramGB))
		}
		stock, stockKnown := inventory.total(ch.zones, ch.name)
		imageUnsupported := len(supported) > 0 && !containsFold(supported, ch.name)
		poolUnsupported, poolReason := poolUnsupportedEverywhere(wfCtx, ch.zones, ch.name)
		canCreate, creatabilityKnown := gpuModelCreatable(combos, ch.name, ch.zones)
		soldOut := creatabilityKnown && !canCreate && anyModelCreatable
		disabled := !ch.normal || imageUnsupported || poolUnsupported || soldOut
		// A disabled option's reason goes in Reason ONLY, never also into the note.
		// The client renders [Note, Disabled && Reason] joined (MessageItem.jsx), so
		// a reason present in both is printed twice — live: "4090 · 该可用区不支持独占
		// 购买方式 · 该可用区不支持独占购买方式". Note carries the neutral context,
		// Reason carries the why.
		disabledReason := ""
		if imageUnsupported {
			disabledReason = "镜像不支持当前 GPU"
		}
		if poolUnsupported && disabledReason == "" {
			// Suppresses the stock line rather than joining it: a card that says both
			// "不支持抢占式" and "库存快照约 3 张" is telling the user to try anyway.
			disabledReason = poolReason
			stockKnown = false
		}
		if soldOut && disabledReason == "" {
			disabledReason = "当前配置在所有可用区都无可创建库存"
			stockKnown = false
		}
		if ch.normal && !poolUnsupported && !soldOut {
			switch {
			case creatabilityKnown && canCreate:
				// The authoritative answer, for the exact image / disk / charge type
				// this create will send. It outranks the snapshot count, which reports
				// 0 for zones that are in fact selling.
				noteParts = append(noteParts, "当前可创建")
			case stockKnown:
				noteParts = append(noteParts, guidedStockNote(stock))
			default:
				// Status==Normal means the model is ON SALE in the catalog — it is NOT
				// evidence that stock exists right now, and no capacity reading is
				// available. Saying "可售" turned that catalog fact into an availability
				// promise, which is how a sold-out GPU was offered as available. State
				// the catalog fact and defer the verdict.
				noteParts = append(noteParts, "在售，库存以最终确认为准")
			}
		}
		if !ch.normal && disabledReason == "" {
			disabledReason = "暂不可售"
		}
		if len(ch.zones) > 0 {
			noteParts = append(noteParts, "可用区 "+strings.Join(ch.zones, "、"))
		}
		opt := ConfirmFormOption{
			Value:    ch.name,
			Label:    label,
			Note:     strings.Join(noteParts, " · "),
			Reason:   disabledReason,
			Disabled: disabled,
			Meta: map[string]string{
				"Sellable": strconv.FormatBool(ch.normal),
			},
		}
		if stockKnown {
			opt.Meta["StockKnown"] = "true"
			opt.Meta["StockFree"] = fmt.Sprintf("%.0f", stock)
		}
		opts = append(opts, opt)
	}
	if current != "" {
		appendChoice(current)
	}
	for _, name := range order {
		if name == current {
			continue
		}
		appendChoice(name)
	}
	selected := current
	if selected == "" || !enabledOptionExists(opts, selected) {
		if value := firstEnabledValue(opts); value != "" {
			selected = value
		} else if selected == "" && len(opts) > 0 {
			selected = opts[0].Value
		}
	}
	return selected, opts
}

func guidedCandidateGPUSet(params map[string]any) ([]string, map[string]bool) {
	raw, ok := params["GuidedCandidateGPUs"]
	if !ok || raw == nil {
		return nil, nil
	}
	var order []string
	switch v := raw.(type) {
	case []string:
		order = append(order, v...)
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				order = append(order, s)
			}
		}
	}
	if len(order) == 0 {
		return nil, nil
	}
	set := map[string]bool{}
	var cleaned []string
	for _, name := range order {
		name = strings.TrimSpace(name)
		if name == "" || set[strings.ToLower(name)] {
			continue
		}
		set[strings.ToLower(name)] = true
		cleaned = append(cleaned, name)
	}
	return cleaned, set
}

func guidedGPUReasons(params map[string]any) map[string]string {
	out := map[string]string{}
	raw, ok := params["GuidedGpuReasons"]
	if !ok || raw == nil {
		return out
	}
	switch v := raw.(type) {
	case map[string]string:
		for k, val := range v {
			if strings.TrimSpace(k) != "" && strings.TrimSpace(val) != "" {
				out[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(val)
			}
		}
	case map[string]any:
		for k, val := range v {
			if s, ok := val.(string); ok && strings.TrimSpace(k) != "" && strings.TrimSpace(s) != "" {
				out[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(s)
			}
		}
	}
	return out
}

func guidedGPUIntentMatches(intent, candidate string) bool {
	return strings.EqualFold(strings.TrimSpace(intent), strings.TrimSpace(candidate))
}

// guidedZoneFormOptions returns the zone card's selection, its options, and
// whether the gates stood down — i.e. whether every zone failed some rule and the
// options were re-enabled to keep the flow moving. The caller needs that flag
// because a stood-down card must say so; silently offering everything is how the
// user ends up choosing a zone the create gate will refuse.
func guidedZoneFormOptions(wfCtx *Context, catalog map[string]any, gpuType, current string, params map[string]any, inventoryResult map[string]any) (string, []ConfirmFormOption, bool) {
	if catalog == nil || gpuType == "" {
		return "", nil, false
	}
	inventory := guidedInventoryFrom(wfCtx, inventoryResult)
	// Real creatability per zone, when the probe managed to establish it. A zone
	// missing from this map was never established and keeps its old behavior —
	// the raw GPU inventory count is NOT a substitute (it reports 0 for zones
	// that are in fact selling, which is why it may inform the note but never
	// disable an option).
	creatable := zoneCreatabilityFor(
		comboCreatability(wfCtx.Result(zoneCapacityStepName)), gpuType, guidedCandidateZones(catalog, gpuType))
	seen := map[string]bool{}
	var opts []ConfirmFormOption
	for _, zone := range guidedCandidateZones(catalog, gpuType) {
		if seen[zone] {
			continue
		}
		seen[zone] = true
		zoneLabel := zoneDisplayLabel(wfCtx, zone)
		count, stockKnown := inventory.count(zone, gpuType)
		note := fmt.Sprintf("%s 可用", gpuType)
		disabled := false
		disabledReason := ""
		if stockKnown {
			note = fmt.Sprintf("%s · %s", gpuType, guidedStockNote(count))
		}
		if ok, known := creatable[zone]; known {
			if ok {
				note = fmt.Sprintf("%s · 当前可创建", gpuType)
			} else {
				disabled = true
				disabledReason = "该可用区当前无可创建库存"
			}
		}
		// The charge type is settled before this card, so the purchase-mode
		// constraint runs in its natural direction: a zone the billing mode cannot
		// use is grayed out here rather than refused at the create gate. It reads
		// the same fact the gate reads, so it can never deny a zone the gate would
		// have accepted; the converse does not hold — see the stand-down below.
		if unsupported, reason := poolUnsupportedInZone(wfCtx, zone, gpuType); !disabled && unsupported {
			disabled = true
			disabledReason = reason
		}
		// The image is settled several cards before this one, so the container/VM
		// constraint runs in its natural direction here too: a zone that cannot boot
		// the chosen image is grayed out while the user can still act on it.
		//
		// "Never offered a zone the create gate will refuse" is NOT what this
		// establishes, and saying so would be a lie the next reader would trust:
		// an image id this catalog page does not carry passes here and is refused
		// there (imageZoneUnverifiable), and the stand-down below deliberately
		// re-enables everything when nothing is left.
		if rejects, reason := zoneRejectsSelectedImage(wfCtx, zone); !disabled && rejects {
			disabled = true
			disabledReason = reason
		}
		// A disabled zone must not also advertise stock. "4090 · 库存约 8 张" beside
		// "该可用区不支持独占购买方式" reads as an invitation to try anyway; the
		// reason is the only thing worth saying. It lives in Reason alone — the
		// client joins [Note, Disabled && Reason], so repeating it here printed it
		// twice.
		if disabled {
			note = gpuType
		}
		opt := ConfirmFormOption{
			Value:    zone,
			Label:    zoneLabel,
			Note:     note,
			Reason:   disabledReason,
			Disabled: disabled,
			Meta:     map[string]string{"Zone": zone, "ZoneLabel": zoneLabel},
		}
		if stockKnown {
			opt.Meta["StockKnown"] = "true"
			opt.Meta["StockFree"] = fmt.Sprintf("%.0f", count)
		}
		if zone == current {
			opts = append([]ConfirmFormOption{opt}, opts...)
		} else {
			opts = append(opts, opt)
		}
	}
	// ONE stand-down over the COMBINED verdict, not one per rule.
	//
	// The gates steer; they do not refuse. Graying out every zone leaves a card
	// that offers nothing, and ensureGuidedZone turns that into "暂无可选可用区" —
	// a dead end raised BEFORE the draft exists, so the failure record would lose
	// both the candidate draft and the typed capacity_sold_out reason the sold-out
	// reply is built from. When nothing is selectable there is nothing to steer
	// toward, and the authoritative negative belongs to 检查库存 / the create gate,
	// which raise it with a complete record. See stepCheckCapacity.
	//
	// This used to be one stand-down per rule, and that is not the same thing.
	// Capacity asked "is any zone creatable", purchase mode asked nothing at all,
	// and the image rule asked "does any zone accept this image" — so a card where
	// capacity killed zone A and the image killed zone B saw each rule find its own
	// survivor, stood down neither, and went out with nothing enabled. Every rule
	// was individually satisfied and the card was still a dead end. Deciding it once
	// on the assembled options is what makes a fourth rule safe to add.
	//
	// What this is NOT: a fix for the user's situation. Re-enabling the options is a
	// downgrade that keeps the flow moving, and one of the known-bad zones then
	// becomes the default. The confirm protocol has no back operation
	// (ConfirmResolution is confirmed/denied plus overrides — types.go), so from
	// here the user can only cancel the whole create and start over, or continue and
	// be stopped by 检查库存 / the create gate. The notes and the stood-down
	// description below are what make that choice an informed one; they are not a
	// substitute for a zone-step structured conflict that could send them back to
	// the image card.
	stoodDown := false
	if firstEnabledValue(opts) == "" && len(opts) > 0 {
		stoodDown = true
		for i := range opts {
			foldReasonIntoNote(&opts[i])
			opts[i].Disabled = false
		}
	}
	selected := current
	if selected == "" || !seen[selected] || !enabledOptionExists(opts, selected) {
		selected = firstEnabledValue(opts)
	}
	return selected, opts, stoodDown
}

// foldReasonIntoNote moves a disabled option's Reason into its Note.
//
// The client renders the reason ONLY while the option is disabled —
// `[o.Note, o.Disabled && o.Reason].filter(Boolean).join(' · ')` — so re-enabling
// an option without folding would silently drop the one line that explained it.
// This is the whole reason Note and Reason are kept disjoint at every producer:
// they can be concatenated safely exactly once, here.
func foldReasonIntoNote(opt *ConfirmFormOption) {
	if opt.Reason == "" {
		return
	}
	if opt.Note == "" {
		opt.Note = opt.Reason
	} else {
		opt.Note = opt.Note + " · " + opt.Reason
	}
	opt.Reason = ""
}

// guidedZoneStandDownDescription replaces the zone card's normal copy when every
// zone failed some rule. The normal copy ("建议优先选择有现货的可用区") is actively
// wrong then — there is no good choice to steer toward — and a card whose options
// each carry a warning but whose heading still recommends picking one reads as a
// rendering glitch rather than as the conflict it is.
func guidedZoneStandDownDescription() string {
	return "当前没有同时满足库存、购买方式和所选镜像的可用区，下面每个区都标注了原因。" +
		"继续确认不会跳过后续检查——真正的创建仍会被拦下。" +
		"建议取消本次创建，改用其他镜像、GPU 型号或计费方式后重新开始。"
}

func guidedGPUCountFormOptions(wfCtx *Context, catalog map[string]any, gpuType, zone string, current float64, params map[string]any, inventoryResult map[string]any) (float64, []ConfirmFormOption) {
	if catalog == nil || gpuType == "" || zone == "" {
		return 0, nil
	}
	inventory := guidedInventoryFrom(wfCtx, inventoryResult)
	capSpecs := parseCapacitySpecs(wfCtx.Result(capacitySpecsStepName))
	seen := map[string]bool{}
	var opts []ConfirmFormOption
	types, _ := catalog["AvailableInstanceTypes"].([]any)
	for _, t := range types {
		mt, _ := t.(map[string]any)
		if name, _ := mt["Name"].(string); name != gpuType {
			continue
		}
		if status, _ := mt["Status"].(string); status != "" && !strings.EqualFold(status, "Normal") {
			continue
		}
		entryZone, _ := mt["Zone"].(string)
		if entryZone == "" {
			continue
		}
		if entryZone != zone {
			continue
		}
		sizes, _ := mt["MachineSizes"].([]any)
		for _, s := range sizes {
			size, _ := s.(map[string]any)
			gpu, _ := size["Gpu"].(float64)
			if gpu <= 0 {
				continue
			}
			value := fmt.Sprintf("%.0f", gpu)
			if seen[value] {
				continue
			}
			seen[value] = true
			free, stockKnown := inventory.count(zone, gpuType)
			zoneLabel := zoneDisplayLabel(wfCtx, zone)
			note := fmt.Sprintf("%s · %s", gpuType, zoneLabel)
			disabled := false
			disabledReason := ""
			if stockKnown {
				fit := guidedStockFitNote(free, gpu)
				note = fmt.Sprintf("%s · %s · %s", gpuType, zoneLabel, fit)
			}
			// Gate by real creatability: disable a card count only when the capacity
			// check enumerated it and found it short. Counts capacity never evaluated
			// stay enabled — the final 检查库存 re-check is the authoritative negative.
			if capacityHasSignal(capSpecs) && capacityKnowsGPUCount(capSpecs, int(gpu)) && !capacityGPUCountEnough(capSpecs, int(gpu)) {
				disabled = true
				disabledReason = "该卡数当前无可创建库存"
			}
			opt := ConfirmFormOption{
				Value:    value,
				Label:    fmt.Sprintf("%.0f 张 GPU", gpu),
				Note:     note,
				Reason:   disabledReason,
				Disabled: disabled,
				Meta:     map[string]string{"GPU": value, "Zone": zone, "ZoneLabel": zoneLabel},
			}
			if stockKnown {
				opt.Meta["StockKnown"] = "true"
				opt.Meta["StockFree"] = fmt.Sprintf("%.0f", free)
			}
			if current == gpu {
				opts = append([]ConfirmFormOption{opt}, opts...)
			} else {
				opts = append(opts, opt)
			}
		}
	}
	// A capacity gate must never dead-end the flow: if it disabled every count,
	// keep them selectable and let the authoritative 检查库存 report the shortage.
	if firstEnabledValue(opts) == "" {
		for i := range opts {
			opts[i].Disabled = false
			opts[i].Reason = ""
		}
	}
	selected := current
	if selected == 0 || !seen[fmt.Sprintf("%.0f", selected)] || !enabledOptionExists(opts, fmt.Sprintf("%.0f", selected)) {
		if value := firstEnabledValue(opts); value != "" {
			selected, _ = strconv.ParseFloat(value, 64)
		} else {
			selected = 0
		}
	}
	return selected, opts
}

func guidedCpuMemoryFormOptions(wfCtx *Context, catalog map[string]any, gpuType, zone string, gpuCount float64, params map[string]any, inventoryResult map[string]any) (string, []ConfirmFormOption) {
	if catalog == nil || gpuType == "" || zone == "" || gpuCount <= 0 {
		return "", nil
	}
	inventory := guidedInventoryFrom(wfCtx, inventoryResult)
	capSpecs := parseCapacitySpecs(wfCtx.Result(capacitySpecsStepName))
	current := ""
	if _, hasCPU := params["Cpu"]; hasCPU {
		if _, hasMem := params["Memory"]; hasMem {
			current = formatGuidedSpecKey(zone, gpuCount, paramNum(params, "Cpu", 0), paramNum(params, "Memory", 0))
		}
	}
	seen := map[string]bool{}
	var opts []ConfirmFormOption
	types, _ := catalog["AvailableInstanceTypes"].([]any)
	for _, t := range types {
		mt, _ := t.(map[string]any)
		if name, _ := mt["Name"].(string); name != gpuType {
			continue
		}
		if status, _ := mt["Status"].(string); status != "" && !strings.EqualFold(status, "Normal") {
			continue
		}
		entryZone, _ := mt["Zone"].(string)
		if entryZone == "" {
			continue
		}
		if entryZone != zone {
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
				if cpu <= 0 {
					continue
				}
				mems, _ := col["Memory"].([]any)
				for _, m := range mems {
					memGB, _ := m.(float64)
					if memGB <= 0 {
						continue
					}
					memMB := memGB * 1024
					key := formatGuidedSpecKey(zone, gpu, cpu, memMB)
					if seen[key] {
						continue
					}
					seen[key] = true
					if current == "" {
						current = key
					}
					free, stockKnown := inventory.count(zone, gpuType)
					zoneLabel := zoneDisplayLabel(wfCtx, zone)
					note := fmt.Sprintf("%s · %.0f 张 GPU · %s", gpuType, gpu, zoneLabel)
					disabled := false
					disabledReason := ""
					if stockKnown {
						fit := guidedStockFitNote(free, gpu)
						note = fmt.Sprintf("%s · %.0f 张 GPU · %s · %s", gpuType, gpu, zoneLabel, fit)
					}
					// Gate by real creatability: disable a CPU/内存 combo only when the
					// capacity check enumerated it and found it short; combos it never
					// evaluated stay enabled (the final 检查库存 is authoritative).
					if capacityHasSignal(capSpecs) && capacityKnowsCombo(capSpecs, int(gpu), int(cpu), int(memGB)) && !capacityCPUMemEnough(capSpecs, int(gpu), int(cpu), int(memGB)) {
						disabled = true
						disabledReason = "该规格当前无可创建库存"
					}
					opts = append(opts, ConfirmFormOption{
						Value:    key,
						Label:    fmt.Sprintf("%.0f 核 CPU · %.0fGB 内存", cpu, memGB),
						Note:     note,
						Reason:   disabledReason,
						Disabled: disabled,
						Meta: map[string]string{
							"Zone":      zone,
							"ZoneLabel": zoneLabel,
							"GPU":       fmt.Sprintf("%.0f", gpu),
							"CPU":       fmt.Sprintf("%.0f", cpu),
							"MemoryGB":  fmt.Sprintf("%.0f", memGB),
						},
					})
					if stockKnown {
						opts[len(opts)-1].Meta["StockKnown"] = "true"
						opts[len(opts)-1].Meta["StockFree"] = fmt.Sprintf("%.0f", free)
					}
				}
			}
		}
	}
	// A capacity gate must never dead-end the flow: if it disabled every combo,
	// keep them selectable and let the authoritative 检查库存 report the shortage.
	if firstEnabledValue(opts) == "" {
		for i := range opts {
			opts[i].Disabled = false
			opts[i].Reason = ""
		}
	}
	if current != "" && (!seen[current] || !enabledOptionExists(opts, current)) {
		current = firstEnabledValue(opts)
	}
	if current == "" {
		current = firstEnabledValue(opts)
	}
	return current, opts
}

func formatGuidedSpecKey(zone string, gpu, cpu, memoryMB float64) string {
	return fmt.Sprintf("%s|%.0f|%.0f|%.0f", zone, gpu, cpu, memoryMB)
}

func parseGuidedSpecKey(key string) (zone string, gpu, cpu, memoryMB float64, err error) {
	parts := strings.Split(key, "|")
	if len(parts) != 4 {
		return "", 0, 0, 0, fmt.Errorf("规格选择无效")
	}
	gpu, err = strconv.ParseFloat(parts[1], 64)
	if err != nil || gpu <= 0 {
		return "", 0, 0, 0, fmt.Errorf("规格选择无效")
	}
	cpu, err = strconv.ParseFloat(parts[2], 64)
	if err != nil || cpu <= 0 {
		return "", 0, 0, 0, fmt.Errorf("规格选择无效")
	}
	memoryMB, err = strconv.ParseFloat(parts[3], 64)
	if err != nil || memoryMB <= 0 {
		return "", 0, 0, 0, fmt.Errorf("规格选择无效")
	}
	zone = strings.TrimSpace(parts[0])
	if zone == "" {
		return "", 0, 0, 0, fmt.Errorf("规格选择无效")
	}
	return zone, gpu, cpu, memoryMB, nil
}

// guidedImageFormOptions returns the picker's current value, its options and the
// TOTAL number of candidates those options are a page of. The total is returned
// rather than inferred because the options are capped at maxGuidedImageOptions:
// the caller states "共 N 个" from the same set the options came from, so the card
// can no longer advertise a population it does not show.
func guidedImageFamilyFormOptionsForContext(wfCtx *Context) (string, []ConfirmFormOption, int) {
	if wfCtx == nil {
		return "", nil, 0
	}
	return guidedImageFamilyFormOptions(
		wfCtx.Params,
		createImageFamilies(wfCtx),
		paramStr(wfCtx.Params, "GpuType", ""),
	)
}

func guidedImageFamilyFormOptions(params map[string]any, families []deployment.ImageFamily, gpuType string) (string, []ConfirmFormOption, int) {
	current := strings.TrimSpace(paramStr(params, "ImageFamily", ""))
	total := 0
	seen := map[string]bool{}
	var opts []ConfirmFormOption
	for _, family := range families {
		if family.Key == "" || seen[family.Key] {
			continue
		}
		name := strings.TrimSpace(family.Name)
		if name == "" && len(family.Variants) > 0 {
			name = family.Variants[0].FamilyLabel()
		}
		if name == "" {
			continue
		}
		seen[family.Key] = true
		total++
		disabled := false
		reason := ""
		if gpuType != "" && len(family.Variants) > 0 {
			anySupported := false
			for _, variant := range family.Variants {
				if len(variant.SupportedGPUTypes) == 0 || containsFold(variant.SupportedGPUTypes, gpuType) {
					anySupported = true
					break
				}
			}
			if !anySupported {
				disabled = true
				reason = "该系列没有支持当前 GPU 的版本"
			}
		}
		if len(opts) >= maxGuidedImageOptions {
			continue
		}
		opts = append(opts, ConfirmFormOption{
			Value:    family.Key,
			Label:    name,
			Note:     fmt.Sprintf("%d 个可选版本", len(family.Variants)),
			Reason:   reason,
			Disabled: disabled,
			Meta: map[string]string{
				"ImageFamily":  family.Key,
				"VariantCount": strconv.Itoa(len(family.Variants)),
			},
		})
	}
	if current == "" || !enabledOptionExists(opts, current) {
		current = firstEnabledValue(opts)
	}
	return current, opts, total
}

func selectedImageFamily(wfCtx *Context) (deployment.ImageFamily, bool) {
	if wfCtx == nil {
		return deployment.ImageFamily{}, false
	}
	key := strings.TrimSpace(paramStr(wfCtx.Params, "ImageFamily", ""))
	if key == "" {
		// A recommended exact community image already names its family through the
		// verified catalog row. It does not need (and must not synthesize) a prior
		// family-card submission merely to render a version picker honestly.
		if scope, ok := currentRecommendedCommunityImageScope(wfCtx); ok {
			key = scope.familyKey
		}
	}
	if key == "" {
		return deployment.ImageFamily{}, false
	}
	for _, family := range createImageFamilies(wfCtx) {
		if family.Key == key {
			return family, true
		}
	}
	return deployment.ImageFamily{}, false
}

func guidedImageFormOptions(params map[string]any, images map[string]any, gpuType string, taxonomy *deployment.ImageTaxonomy, zoneIsPod bool) (string, []ConfirmFormOption, int) {
	if images == nil {
		return "", nil, 0
	}
	set := buildImageCandidateSet(params, images, gpuType, taxonomy, zoneIsPod)
	return guidedImageFormOptionsFromSet(params, images, gpuType, set, false)
}

// guidedImageFormOptionsForContext is the context-aware picker path. When the
// Agent omitted ImageName (or supplied only a free-text suggestion) but the current
// turn literally matches one framework or tag in the live catalog,
// createImageCandidates supplies that structured request and the generic catalog
// default is suppressed. The ranked catalog candidates therefore lead; no concrete
// id is treated as current until the user submits this picker.
func guidedImageFormOptionsForContext(wfCtx *Context, gpuType string) (string, []ConfirmFormOption, int) {
	if wfCtx == nil {
		return "", nil, 0
	}
	images := createImageResult(wfCtx)
	if images == nil {
		return "", nil, 0
	}
	_, catalogFallback := currentTurnImageCatalogRequest(wfCtx)
	return guidedImageFormOptionsFromSet(
		wfCtx.Params,
		images,
		gpuType,
		createImageCandidates(wfCtx),
		catalogFallback,
	)
}

func guidedImageFormOptionsFromSet(params map[string]any, images map[string]any, gpuType string, set imageCandidateSet, suppressCatalogDefault bool) (string, []ConfirmFormOption, int) {
	snap := set.snap
	ranked := set.final
	wantType := strings.TrimSpace(paramStr(params, "ImageType", ""))
	wantTag := strings.TrimSpace(paramStr(params, "ImageTag", ""))

	// total counts every DISTINCT candidate this card is a page of, including the
	// threaded current selection when it is not one of the ranked rows. It is
	// counted here rather than from len(opts) because opts stops at the page size —
	// which is the whole bug this returns a total to close.
	total := 0
	counted := map[string]bool{}
	seen := map[string]bool{}
	var opts []ConfirmFormOption
	appendOpt := func(id, label string, supported []string) {
		if id == "" {
			return
		}
		if !counted[id] {
			counted[id] = true
			total++
		}
		if seen[id] || len(opts) >= maxGuidedImageOptions {
			return
		}
		seen[id] = true
		note, reason := "", ""
		disabled := false
		// A GPU-recommendation mismatch is shown DISABLED (not hidden), so the user
		// sees why an image they might name is not selectable for this card.
		// Reason only — the client joins [Note, Disabled && Reason], so the old pair
		// ("所选镜像不支持当前 GPU" + "镜像不支持当前 GPU") rendered as the same
		// sentence twice with a separator between them.
		if gpuType != "" && len(supported) > 0 && !containsFold(supported, gpuType) {
			reason = "镜像不支持当前 GPU"
			disabled = true
		}
		opts = append(opts, ConfirmFormOption{
			Value: id, Label: label, Note: note, Reason: reason, Disabled: disabled,
			Meta: map[string]string{"ImageId": id},
		})
	}

	// Membership in the HARD-filtered candidate set (set.base is post-status,
	// post-pod/container; it does not yet apply the type/tag facets). The
	// current/threaded lead below must respect this: pickImageId resolves a default
	// from the raw catalog with NO pod constraint, so on a pod zone it can name a
	// VM-only image the candidate set already dropped. Leading with it re-added the
	// very image RankImages excluded — which is how a pinned pod zone still offered
	// "Ubuntu-nvidia 22.04" and the create gate refused it at the end.
	inCandidates := map[string]bool{}
	for _, sel := range set.base {
		inCandidates[sel.ID] = true
	}

	// The current/threaded selection leads — but only if it survives the active
	// facets AND the hard filters. A selection dropped by a facet is re-picked from
	// the facet-scoped candidates below; a selection dropped by the pod/status gate
	// is not a valid candidate at all and must not lead (or appear).
	current := ""
	if !suppressCatalogDefault {
		current = pickImageId(params, images)
	}
	if current != "" && imageSelectionMatchesFacets(snap, current, wantType, wantTag) {
		if entry, ok := snap.ByID(current); ok {
			if inCandidates[current] {
				appendOpt(entry.ID, entry.DisplayLabel(), entry.SupportedGPUTypes)
			} else {
				// Present in the catalog but dropped by a hard filter (e.g. a VM image
				// in a pod zone). Not a candidate — fall through to the ranked set.
				current = ""
			}
		} else {
			// An exact id reaches this picker only through the live catalog. The
			// workflow merges a resolver-verified page-out row before this function;
			// absence here therefore means it is not safe to offer.
			current = ""
		}
	} else {
		current = ""
	}
	for _, sel := range ranked {
		entry, ok := snap.ByID(sel.ID)
		// Label from the catalog row so two versions of one family are told apart by
		// their version; the ranked candidate carries only the (shared) family name.
		label := sel.Name
		if ok {
			if l := entry.DisplayLabel(); l != "" {
				label = l
			}
		}
		appendOpt(sel.ID, label, entry.SupportedGPUTypes)
	}
	if len(opts) == 0 {
		return "", nil, 0
	}
	if current == "" || !seen[current] || !enabledOptionExists(opts, current) {
		current = firstEnabledValue(opts)
	}
	return current, opts, total
}

// gpuFormOptions lists selectable GPU types: sellable types from the catalog,
// filtered to the current image's SupportedGpuTypes when declared, current
// type first. No stock claim is attached — stock is only asserted by the
// post-edit 检查库存 re-run.
func gpuFormOptions(catalog map[string]any, supported []string, current string) []ConfirmFormOption {
	if catalog == nil {
		return nil
	}
	opts := []ConfirmFormOption{{Value: current, Label: gpuOptionLabel(catalog, current)}}
	seen := map[string]bool{current: true}
	types, _ := catalog["AvailableInstanceTypes"].([]any)
	for _, t := range types {
		if len(opts) >= maxFormGPUOptions {
			break
		}
		mt, _ := t.(map[string]any)
		name, _ := mt["Name"].(string)
		if name == "" || seen[name] {
			continue
		}
		if status, _ := mt["Status"].(string); status != "" && status != "Normal" {
			continue
		}
		if len(supported) > 0 && !containsFold(supported, name) {
			continue
		}
		seen[name] = true
		opts = append(opts, ConfirmFormOption{Value: name, Label: gpuOptionLabel(catalog, name)})
	}
	return opts
}

// gpuOptionLabel renders "4090（24G显存）" when the catalog carries VRAM info,
// else just the type name.
func gpuOptionLabel(catalog map[string]any, gpuType string) string {
	types, _ := catalog["AvailableInstanceTypes"].([]any)
	for _, t := range types {
		mt, _ := t.(map[string]any)
		if name, _ := mt["Name"].(string); name != gpuType {
			continue
		}
		gm, _ := mt["GraphicsMemory"].(map[string]any)
		if v, _ := gm["Value"].(float64); v > 0 {
			return fmt.Sprintf("%s（%.0fG显存）", gpuType, v)
		}
		break
	}
	return gpuType
}

// zoneFormOptions builds the confirm-card zone selector, listing the zones the
// current GPU type is sellable in with the current zone first. Each option's
// display label comes from the turn snapshot (via zoneDisplayLabel) — the single
// zone authority — degrading to the bare zone id, never a legacy per-zone label map.
func zoneFormOptions(wfCtx *Context, catalog map[string]any, gpuType, current string) []ConfirmFormOption {
	if catalog == nil {
		return nil
	}
	opts := []ConfirmFormOption{{Value: current, Label: zoneDisplayLabel(wfCtx, current)}}
	seen := map[string]bool{current: true}
	types, _ := catalog["AvailableInstanceTypes"].([]any)
	for _, t := range types {
		mt, _ := t.(map[string]any)
		if name, _ := mt["Name"].(string); name != gpuType {
			continue
		}
		z, _ := mt["Zone"].(string)
		if z == "" || seen[z] {
			continue
		}
		if status, _ := mt["Status"].(string); status != "" && status != "Normal" {
			continue
		}
		seen[z] = true
		opts = append(opts, ConfirmFormOption{Value: z, Label: zoneDisplayLabel(wfCtx, z)})
	}
	return opts
}

// zoneDisplayLabel is the display-lenient view of workflowZoneEntry: it shows the
// zone's console name, degrading to the bare zone id on any resolution failure
// (an option a form should not have offered — see the Entry gate). It never
// fails, because a label is display-only.
func zoneDisplayLabel(wfCtx *Context, zone string) string {
	if entry, err := workflowZoneEntry(wfCtx, zone); err == nil && strings.TrimSpace(entry.DisplayName) != "" {
		return entry.DisplayName
	}
	return zone
}

// imageFormOptions returns the currently selected image id plus up to
// maxFormImageOptions recommendations from the queried source (no cross-source
// mixing), filtered to images that declare support for the current GPU type
// (or declare no constraint). The current selection is always first.
func imageFormOptions(params map[string]any, images map[string]any, gpuType string, taxonomy *deployment.ImageTaxonomy) (string, []ConfirmFormOption) {
	current := pickImageId(params, images)
	if current == "" || images == nil {
		return "", nil
	}
	snap := formImageCatalog(images, paramStr(params, "ImageSource", "platform"))
	zoneIsPod := paramBool(params, "ZoneIsPod", false) || paramBool(params, "IsPodZone", false)
	ranked := deployment.RankImages(snap, deployment.ImageRequest{
		Name:         paramStr(params, "ImageName", ""),
		RequestedGPU: gpuType,
		Zone:         deployment.ZoneConstraint{Zone: paramStr(params, "Zone", ""), IsPod: zoneIsPod},
	})
	// Narrow by the optional ImageType / ImageTag facets (unset facet = no filter).
	ranked = filterImagesByFacets(snap, ranked, params, taxonomy)
	wantType := strings.TrimSpace(paramStr(params, "ImageType", ""))
	wantTag := strings.TrimSpace(paramStr(params, "ImageTag", ""))

	opts := []ConfirmFormOption{}
	seen := map[string]bool{}
	// Unlike the guided form (which disables mismatches), this list FILTERS out
	// GPU-mismatch / non-container-in-pod images — it only offers selectable ones.
	appendOpt := func(id, label string, supported []string, container bool) {
		if id == "" || seen[id] || len(opts) >= maxFormImageOptions {
			return
		}
		if zoneIsPod && !container {
			return
		}
		if len(supported) > 0 && !containsFold(supported, gpuType) {
			return
		}
		seen[id] = true
		opts = append(opts, ConfirmFormOption{Value: id, Label: label})
	}

	// Current selection first (from the raw result, so a threaded id absent from the
	// ranked pool is still honored) — but only when it survives the active facets.
	if !imageSelectionMatchesFacets(snap, current, wantType, wantTag) {
		current = ""
	} else if entry, ok := snap.ByID(current); ok {
		appendOpt(entry.ID, entry.Name, entry.SupportedGPUTypes, entry.Container)
	} else {
		current = ""
	}
	if current != "" && !seen[current] {
		current = ""
	}
	for _, sel := range ranked {
		entry, _ := snap.ByID(sel.ID)
		appendOpt(sel.ID, sel.Name, entry.SupportedGPUTypes, entry.Container)
	}
	if current == "" && len(opts) > 0 {
		current = opts[0].Value
	}
	return current, opts
}

// currentImageSupportedGPUs returns the SupportedGpuTypes declared by the
// currently selected image (empty = no constraint declared).
func currentImageSupportedGPUs(params map[string]any, images map[string]any) []string {
	if images == nil {
		return nil
	}
	if strings.EqualFold(strings.TrimSpace(paramStr(params, "ImageSource", "")), "community") &&
		strings.TrimSpace(paramStr(params, "CompShareImageId", "")) == "" &&
		strings.TrimSpace(paramStr(params, "ImageName", "")) == "" {
		return nil
	}
	if id := pickImageId(params, images); id != "" {
		if s := imageSupportedByID(images, id); len(s) > 0 {
			return s
		}
	}
	return nil
}

// imageNameByID finds the display name for an image id in either result shape
// (platform ImageSet / community CompshareImageGroup). Returns "" when absent.
func imageNameByID(images map[string]any, id string) string {
	if images == nil || id == "" {
		return ""
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
					name, _ := gm["ImageName"].(string)
					return name
				}
			}
		}
		return ""
	}
	imageSet, _ := images["ImageSet"].([]any)
	for _, item := range imageSet {
		img, _ := item.(map[string]any)
		if img == nil {
			continue
		}
		if got, _ := img["CompShareImageId"].(string); got == id {
			name, _ := img["Name"].(string)
			return name
		}
	}
	return ""
}

// imageSupportedByID returns the SupportedGpuTypes list of the image with the
// given id, in either result shape.
func imageSupportedByID(images map[string]any, id string) []string {
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
					return formStringSlice(dm["SupportedGpuTypes"])
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
			return formStringSlice(img["SupportedGpuTypes"])
		}
	}
	return nil
}

func imageContainerByID(images map[string]any, id string) bool {
	if images == nil || id == "" {
		return false
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
					return paramBool(dm, "Container", false) || paramBool(dm, "IsContainer", false)
				}
			}
		}
		return false
	}
	imageSet, _ := images["ImageSet"].([]any)
	for _, item := range imageSet {
		img, _ := item.(map[string]any)
		if img == nil {
			continue
		}
		if got, _ := img["CompShareImageId"].(string); got == id {
			return paramBool(img, "Container", false) || paramBool(img, "IsContainer", false)
		}
	}
	return false
}

// formStringSlice converts a JSON-decoded []any of strings to []string,
// skipping non-string and duplicate entries.
func formStringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	seen := make(map[string]bool, len(arr))
	var out []string
	for _, x := range arr {
		s, ok := x.(string)
		if !ok || s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// containsFold reports whether list contains s, case-insensitively.
func containsFold(list []string, s string) bool {
	for _, item := range list {
		if strings.EqualFold(item, s) {
			return true
		}
	}
	return false
}
