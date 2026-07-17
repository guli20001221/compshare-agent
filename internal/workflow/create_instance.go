package workflow

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/compshare-agent/internal/deployment"
)

// defaultZone is the default availability zone per API docs (cn-wlcb-01, not cn-wlcb-a).
const defaultZone = "cn-wlcb-01"

const (
	guidedStepGPU = iota + 1
	guidedStepZone
	guidedStepGPUCount
	guidedStepCPUMemory
	guidedStepImagePurpose
	guidedStepImage
	guidedStepFinal
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
// otherwise the zone the requested GPU actually lives in (preferring the platform
// default zone, then any "Normal" zone), falling back to defaultZone when the
// catalog carries no zone info. This is what fixes cards that exist only in a
// non-default zone (e.g. 2080Ti in cn-sh2-02) instead of failing with a hardcoded
// cn-wlcb-01 query.
func resolveTargetSpec(wfCtx *Context) (gpu, cpu, memoryMB float64, zone string, err error) {
	gpuType, _ := wfCtx.Params["GpuType"].(string)
	gpu = paramNum(wfCtx.Params, "Gpu", 1)

	result := wfCtx.Result("查询可用配比")
	if result == nil {
		return 0, 0, 0, "", fmt.Errorf("无法确定目标规格（CPU/Memory），「查询可用配比」步骤未返回结果")
	}

	zone = resolveTargetZone(result, gpuType, paramStr(wfCtx.Params, "Zone", ""))

	candidates := listSpecCandidates(result, gpuType, gpu, zone)
	if len(candidates) == 0 {
		// Grounded failure: list the GPU types the catalog actually returned so a
		// downstream reply can state real options instead of fabricating them.
		return 0, 0, 0, "", fmt.Errorf("未找到 %s × %.0f 卡的可用配比。当前可部署的 GPU 机型：%s。请确认机型名称与卡数是否正确。",
			gpuType, gpu, availableTypeNames(result))
	}

	// Zone for the downstream API args. resolveTargetZone returns "" only when the
	// catalog has no zone data (legacy/test results) and the user didn't pick one.
	if zone == "" {
		zone = defaultZone
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
// carry this GPU, preferring a "Normal" (sellable) zone over a sold-out one and
// the platform default zone over others. Returns "" when the catalog has no zone
// data for the type (legacy/test results) so the caller can fall back to defaultZone.
func resolveTargetZone(result map[string]any, gpuType, userZone string) string {
	if userZone != "" {
		return userZone
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
	if z := preferZone(normalZones); z != "" {
		return z
	}
	return preferZone(allZones)
}

// preferZone picks the platform default zone if present, else the first zone.
func preferZone(zones []string) string {
	for _, z := range zones {
		if z == defaultZone {
			return z
		}
	}
	if len(zones) > 0 {
		return zones[0]
	}
	return ""
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
// single resolved zone — avoiding cross-zone duplicates. An empty targetZone, or
// an entry with no Zone field (legacy/test results), matches everything.
func listSpecCandidates(result map[string]any, gpuType string, gpuCount float64, targetZone string) []specCandidate {
	var candidates []specCandidate
	types, _ := result["AvailableInstanceTypes"].([]any)
	for _, t := range types {
		mt, _ := t.(map[string]any)
		name, _ := mt["Name"].(string)
		if name != gpuType {
			continue
		}
		if entryZone, _ := mt["Zone"].(string); targetZone != "" && entryZone != "" && entryZone != targetZone {
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

// CreateInstanceDef returns the 8-step workflow definition for creating a
// CompShare GPU instance.
//
// 形成执行草稿 sits between the queries and 检查库存 because everything after it
// must describe ONE resolution: stock is checked for the draft, price is quoted
// for the draft, the card shows the draft, and the create sends the copy of it
// the user sealed.
func CreateInstanceDef() *Definition {
	return &Definition{
		Name:        "CreateInstanceWorkflow",
		Description: "查询镜像 → 查询可用配比 → 形成执行草稿 → 检查库存 → 查询价格 → 形成确认快照 → 确认 → 创建实例 → 查看状态",
		Steps: []Step{
			stepQueryImages(false),
			stepQueryInstanceTypes(),
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
	}
}

// CreateInstanceGuidedDef returns the guided, Figma-style order flow for
// creating a CompShare GPU instance. The public action name stays
// CreateInstanceWorkflow so old tooling and confirmation labels remain stable.
func CreateInstanceGuidedDef() *Definition {
	return &Definition{
		Name:        "CreateInstanceWorkflow",
		Description: "查询镜像 → 查询可用配比 → 查询GPU库存 → 选择 GPU → 选择可用区 → 选择卡数量 → 选择 CPU/内存 → 选择用途 → 必要时查询社区镜像 → 选择镜像 → 形成执行草稿 → 检查库存 → 查询价格 → 形成确认快照 → 确认镜像计费 → 创建实例 → 查看状态",
		Steps: []Step{
			stepQueryImages(true),
			stepQueryInstanceTypes(),
			stepQueryGPUInventory(),
			stepGuidedChooseGPU(),
			stepGuidedChooseZone(),
			stepGuidedChooseGPUCount(),
			stepGuidedChooseCPUMemory(),
			stepGuidedChooseImagePurpose(),
			stepQueryCommunityImagesAfterPurpose(),
			stepGuidedChooseImage(),
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
				name := paramStr(wfCtx.Params, "ImageName", "")
				if name == "" {
					if allowCommunityBrowse {
						return communityImageBrowseArgs(""), nil
					}
					return nil, fmt.Errorf("使用社区镜像创建实例时必须指定镜像名称（ImageName），请告诉我您想使用哪个社区镜像")
				}
				return map[string]any{"FuzzySearch": name}, nil
			}
			args := map[string]any{
				"Limit": 20,
			}
			if id := paramStr(wfCtx.Params, "CompShareImageId", ""); id != "" {
				args["CompShareImageId"] = id
				return args, nil
			}
			if name := paramStr(wfCtx.Params, "ImageName", ""); name != "" {
				args["Name"] = name
			}
			return args, nil
		},
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

func stepQueryCommunityImagesAfterPurpose() Step {
	return Step{
		Name:   "查询镜像",
		Type:   StepToolCall,
		Tool:   "DescribeCommunityImages",
		SkipIf: shouldSkipCommunityPurposeImageQuery,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			return communityImageBrowseArgs(paramStr(wfCtx.Params, "ImageName", "")), nil
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
			if strings.EqualFold(createChargeType(wfCtx.Params), deployment.ChargeTypeSpot) {
				args["InstanceType"] = "spot"
			}
			return args, nil
		},
	}
}

func stepQueryGPUInventory() Step {
	return Step{
		Name:     "查询GPU库存",
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
			specs, _ := result["Specs"].([]any)
			if len(specs) == 0 {
				return CheckFailed("库存检查未返回任何规格信息，可能当前 GPU 型号不可用。")
			}

			draft, err := candidateCreateDraft(wfCtx)
			if err != nil {
				return CheckFailed(err.Error())
			}
			gpu := draft.Args.GPU
			cpu := draft.Args.CPU
			memGB := draft.Args.Memory / 1024 // Specs.Mem is in GB; the draft's Memory is MB
			gt := draft.Args.GpuType
			if gt == "" {
				gt = "该 GPU"
			}

			// Match the exact GPU/CPU/Mem combination the workflow will create —
			// read off the draft, so "will create" is a fact rather than a claim.
			for _, s := range specs {
				spec, _ := s.(map[string]any)
				sGpu, _ := spec["Gpu"].(float64)
				sCpu, _ := spec["Cpu"].(float64)
				sMem, _ := spec["Mem"].(float64)
				if sGpu == gpu && sCpu == cpu && sMem == memGB {
					if enough, _ := spec["ResourceEnough"].(bool); enough {
						return CheckPassed()
					}
					// The one rejection a caller acts on: this spec is real and
					// upstream has none of it right now, so alternatives are worth
					// offering. The reason is declared HERE, at the branch that knows
					// it, which is what frees the sentence below to be reworded or
					// translated without changing what the engine does.
					return CheckFailedBecause(ReasonCapacitySoldOut,
						fmt.Sprintf("%s %.0f 卡 / %.0fC / %.0fGB 当前库存不足（售罄），请换一个规格或稍后再试。", gt, gpu, cpu, memGB))
				}
			}

			// Deliberately NOT capacity_sold_out: this combination does not exist at
			// all. Offering "other cards in this zone" answers a question the user
			// did not ask — they need their configuration corrected, not substituted.
			return CheckFailed(fmt.Sprintf("库存中未找到 %s %.0f 卡 / %.0fC / %.0fGB 的规格组合，请确认配置是否正确。", gt, gpu, cpu, memGB))
		},
	}
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

func stepGuidedChooseImagePurpose() Step {
	return Step{
		Name:              "选择用途",
		Type:              StepConfirm,
		SkipIf:            shouldSkipGuidedImagePurposeStep,
		BuildForm:         buildGuidedImagePurposeForm,
		ApplyOverrides:    applyGuidedImagePurposeOverrides,
		ConfirmSubmitMode: ConfirmSubmitContinue,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			purpose := imagePurposeValue(wfCtx.Params)
			return map[string]any{
				"workflow":     "CreateInstanceWorkflow",
				"step":         guidedStepLabel(wfCtx, guidedStepImagePurpose),
				"ImagePurpose": purpose,
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
			current, opts := guidedImageFormOptions(wfCtx.Params, wfCtx.Result("查询镜像"), gpuType)
			if len(opts) == 0 {
				return nil, fmt.Errorf("未找到可选社区镜像，请换一个镜像来源或稍后再试")
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
	if placement.IsPod && strings.EqualFold(chargeType, deployment.ChargeTypeSpot) {
		return fmt.Errorf("%s 当前不支持抢占式实例，请改用按量、包日或包月", zoneDisplayLabel(wfCtx, placement.Zone))
	}
	if !placement.IsPod {
		return nil
	}
	if placement.ZoneID == 0 {
		return fmt.Errorf("未获取到 %s 的内部可用区编号，无法安全创建。请稍后重试或到控制台确认可用区", zoneDisplayLabel(wfCtx, placement.Zone))
	}
	if purchase && placement.AzGroup == 0 {
		return fmt.Errorf("未获取到 %s 的内部地域编号，无法安全创建。请稍后重试或到控制台确认可用区", zoneDisplayLabel(wfCtx, placement.Zone))
	}
	return nil
}

func validateSelectedImageCompatibility(wfCtx *Context, imageID string, placement deployment.ZonePlacement) error {
	image := imageMapByID(wfCtx.Result("查询镜像"), imageID)
	name := imageNameByID(wfCtx.Result("查询镜像"), imageID)
	if name == "" {
		name = "所选镜像"
	}
	if image == nil {
		if placement.IsPod {
			return fmt.Errorf("未能确认 %s 是可用于 %s 的容器镜像，请刷新后重新选择", name, zoneDisplayLabel(wfCtx, placement.Zone))
		}
		// Community image searches can return a different page/order on a second
		// query. Keep the exact selected id for normal zones; the upstream capacity
		// preflight validates that id, its status, and its adaptive UHost image.
		return nil
	}
	if status := strings.TrimSpace(paramStr(image, "Status", "")); status != "" && !strings.EqualFold(status, deployment.ImageStatusAvailable) {
		return fmt.Errorf("%s 当前不可用，请更换镜像", name)
	}
	if placement.IsPod && !imageContainerByID(wfCtx.Result("查询镜像"), imageID) {
		return fmt.Errorf("%s 不是容器镜像，不能用于 %s，请更换镜像或可用区", name, zoneDisplayLabel(wfCtx, placement.Zone))
	}
	gpuType := paramStr(wfCtx.Params, "GpuType", "")
	supported := imageSupportedByID(wfCtx.Result("查询镜像"), imageID)
	if gpuType == "" || len(supported) == 0 || containsFold(supported, gpuType) {
		return nil
	}
	return fmt.Errorf("%s 不支持当前 GPU %s，请更换镜像或卡型", name, gpuType)
}

func workflowSystemDisks(wfCtx *Context, imageID, zone, gpuType string) []any {
	return deployment.ResolveBootDisk(wfCtx.Result("查询镜像"), wfCtx.Result("查询可用配比"), imageID, gpuType, zone)
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
// returns — every live capture of GetCompShareInstancePriceResponse uses it
// (eval/reports/real_cli_golden_doubao_lite_runner.md,
// eval/shadow_qa/2026-04-17-real-account-round2/platform_failures_report.md) and
// "Price" appears in none of them. This function's doc used to say the opposite:
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
	image := selectCreateImage(wfCtx.Params, wfCtx.Result("查询镜像"))
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
	// every charge type at once (Postpay/Dynamic/Day/Month/Spot — see
	// eval/real_cli_golden_doubao_lite.md:74-79, the reason this is a no-op on all
	// four charge types the form offers rather than a new failure mode for three of
	// them).
	if snapshot.EstimatedPrice == nil {
		return nil, fmt.Errorf(missingWorkflowPriceMessage)
	}
	// The price the card shows is the snapshot's, verbatim — the same string that
	// gets sealed. It already carries 预估, because upstream cannot hold a price and
	// the frontend that renders this frame is not ours to relabel.
	price := snapshot.EstimatedPrice.DisplayText
	priceNote := createPriceNote
	return map[string]any{
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
	}, nil
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
	if wfCtx.sealed == nil || wfCtx.sealed.Operation != "CreateInstanceWorkflow" {
		return nil, fmt.Errorf("创建实例缺少已确认的执行合同，拒绝以未经确认的参数创建")
	}
	stored, ok := wfCtx.sealed.BusinessParams[createDraftKey].(map[string]any)
	if !ok || len(stored) == 0 {
		return nil, fmt.Errorf("已确认的执行合同中缺少创建参数，拒绝以重新推导的参数创建")
	}
	snapshot, err := ParseCreateConfirmationSnapshot(stored)
	if err != nil {
		return nil, fmt.Errorf("已确认的执行合同无法解析（%v），拒绝以重新推导的参数创建", err)
	}
	// A contract with no price cannot be a create anyone agreed to: the card that
	// forms the agreement cannot be built without one (buildCreateConfirmArgs), so
	// a sealed snapshot missing its estimate did not come from a user saying yes to
	// a price.
	//
	// The codec deliberately allows a priceless snapshot — an absent quote is a
	// real outcome and the encoder must be able to say so — so this is the create's
	// rule, not the format's, and it belongs at the create's own entry. Here rather
	// than at promoteCreateDraft because this is the last thing between a contract
	// and an irreversible call: it catches a priceless contract however it was
	// formed, not only one that came through promote.
	//
	// Unreachable by the current wiring, and that is the point. It is the guard
	// against a future reordering or a second writer reintroducing the confirmed-
	// without-a-price card that 85da9df6 removed from the front door.
	if snapshot.EstimatedPrice == nil {
		return nil, fmt.Errorf("已确认的执行合同中没有价格记录，拒绝创建：用户不可能确认过一个没有价格的下单")
	}
	// Only the execution half. The sealed snapshot also records the estimate the
	// user was shown, which exists for audit and must never reach the API.
	//
	// The executor cannot reach into the frozen record the digest is computed over:
	// the parse above copies the disks out of the sealed map and UpstreamCreateArgs
	// copies them again into the request, so the two share nothing. Until those
	// copies existed this comment was false — the request's disk list WAS the
	// sealed one, and a write through it would have rewritten the audit record with
	// verifyDigest none the wiser, since the digest covers the live Params rather
	// than the frozen copy. It chooses the wire shape and nothing else — every
	// value it uses was sealed.
	return snapshot.Execution.UpstreamCreateArgs(), nil
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

// selectCreateImage resolves the image ONCE. It is the only image decision in the
// create flow; the draft carries the result whole, so the confirm card renders
// Name and the create sends ID without either re-selecting.
func selectCreateImage(params map[string]any, result map[string]any) SelectedImage {
	source := paramStr(params, "ImageSource", "platform")
	if id := paramStr(params, "CompShareImageId", ""); id != "" {
		// The id is already a decision (threaded in by the deploy_model handler or
		// a form override). Prefer the name the catalog gives for THAT id over the
		// ImageName that travelled alongside it: when the two disagree the catalog
		// describes the image that will actually be built.
		return SelectedImage{ID: id, Name: catalogImageName(result, id, params), Source: source}
	}
	if source == "community" {
		return selectCommunityImage(result)
	}
	img := matchPlatformImage(params, result)
	if img == nil {
		return SelectedImage{Source: source}
	}
	id, _ := img["CompShareImageId"].(string)
	name, _ := img["Name"].(string)
	if name == "" {
		name = "未知"
	}
	return SelectedImage{ID: id, Name: name, Source: source}
}

// catalogImageName returns the catalog's display name for an already-chosen id.
//
// It delegates to imageNameByID, which walks EVERY community group and the whole
// platform ImageSet. The first version of this function grew its own community
// lookup off selectCommunityImage — which only reads groups[0] — so an id living
// in the second group or later was reported "not in the catalog" and fell back to
// the threaded name, breaking the very contract this function exists to keep. One
// id→name lookup, not two.
//
// The fallback is only reached when the id is genuinely absent from this response
// (a community id against a platform query, a cached id, an empty result). What it
// returns is a name NOT confirmed against the catalog — the caller is trusting the
// pair it was handed. A future typed image capability should distinguish resolved
// / not-found / catalog-unavailable instead of flattening all three into a string.
func catalogImageName(result map[string]any, id string, params map[string]any) string {
	if name := imageNameByID(result, id); name != "" {
		return name
	}
	return paramStr(params, "ImageName", "")
}

// selectCommunityImage reads the id and the display name off the SAME group, so
// the pair cannot drift apart the way two independent walks could.
func selectCommunityImage(result map[string]any) SelectedImage {
	selected := SelectedImage{Name: "未知", Source: "community"}
	if result == nil {
		return selected
	}
	groups, ok := result["CompshareImageGroup"].([]any)
	if !ok || len(groups) == 0 {
		return selected
	}
	group, ok := groups[0].(map[string]any)
	if !ok {
		return selected
	}
	if name, ok := group["ImageName"].(string); ok && name != "" {
		selected.Name = name
	}
	data, ok := group["Data"].([]any)
	if !ok || len(data) == 0 {
		return selected
	}
	first, ok := data[0].(map[string]any)
	if !ok {
		return selected
	}
	if id, ok := first["CompShareImageId"].(string); ok {
		selected.ID = id
	}
	return selected
}

func pickImageId(params map[string]any, result map[string]any) string {
	if id := paramStr(params, "CompShareImageId", ""); id != "" {
		return id
	}
	if paramStr(params, "ImageSource", "platform") == "community" {
		return pickFirstCommunityImageId(result)
	}
	return pickPlatformImageId(params, result)
}

// pickImageName dispatches to the correct picker based on ImageSource. When an
// image id was threaded (deploy_model handler), the threaded ImageName is the display
// name of that exact image — use it so the confirm shows what actually gets built.
func pickImageName(params map[string]any, result map[string]any) string {
	if paramStr(params, "CompShareImageId", "") != "" {
		if name := paramStr(params, "ImageName", ""); name != "" {
			return name
		}
	}
	if paramStr(params, "ImageSource", "platform") == "community" {
		return pickFirstCommunityImageName(result)
	}
	return pickPlatformImageName(params, result)
}

// --- Platform image helpers ---

// pickPlatformImageId selects a platform image ID using name matching when
// ImageName is provided, falling back to the first result.
func pickPlatformImageId(params map[string]any, result map[string]any) string {
	img := matchPlatformImage(params, result)
	if img == nil {
		return ""
	}
	if id, ok := img["CompShareImageId"].(string); ok {
		return id
	}
	return ""
}

// pickPlatformImageName selects a platform image display name using name
// matching when ImageName is provided, falling back to the first result.
func pickPlatformImageName(params map[string]any, result map[string]any) string {
	img := matchPlatformImage(params, result)
	if img == nil {
		return "未知"
	}
	if name, ok := img["Name"].(string); ok && name != "" {
		return name
	}
	return "未知"
}

// matchPlatformImage returns the best-matching image map from ImageSet.
// Priority: intent/name relevance > newer version > first viable catalog entry.
// Within a name-matched bucket, GPU-supported images rank first via the shared
// deployment selector. SupportedGpuTypes is a ranking hint, not a static zone
// guarantee; the create call remains authoritative for final image adaptation.
func matchPlatformImage(params map[string]any, result map[string]any) map[string]any {
	if result == nil {
		return nil
	}
	imageSet, ok := result["ImageSet"].([]any)
	if !ok || len(imageSet) == 0 {
		return nil
	}

	keyword := paramStr(params, "ImageName", "")
	if keyword == "" {
		if ranked, narrowed := platformImagesForIntent(params, platformImageMaps(imageSet)); narrowed {
			if img := bestViablePlatformImage(params, ranked); img != nil {
				return img
			}
			return nil
		}
		// No name/purpose preference — keep the catalog's default order, only skipping
		// entries the shared selector rejects as unavailable / wrong shape.
		if img := firstViablePlatformImage(params, imageSet); img != nil {
			return img
		}
		return firstUsableImageMap(imageSet)
	}

	maps := platformImageMaps(imageSet)
	if ranked, narrowed := platformImagesForIntent(params, maps); narrowed {
		if img := bestViablePlatformImage(params, ranked); img != nil {
			return img
		}
		return nil
	}

	// No match — fall back to the default platform image.
	if img := firstViablePlatformImage(params, imageSet); img != nil {
		return img
	}
	return firstUsableImageMap(imageSet)
}

func platformImageMaps(imageSet []any) []map[string]any {
	maps := make([]map[string]any, 0, len(imageSet))
	for _, item := range imageSet {
		if img, _ := item.(map[string]any); img != nil {
			maps = append(maps, img)
		}
	}
	return maps
}

func filterPlatformImages(imageSet []any, keyword string, exact bool) []map[string]any {
	var out []map[string]any
	lowerKeyword := strings.ToLower(keyword)
	for _, item := range imageSet {
		img, _ := item.(map[string]any)
		if img == nil {
			continue
		}
		name, _ := img["Name"].(string)
		if exact {
			if strings.EqualFold(name, keyword) {
				out = append(out, img)
			}
			continue
		}
		if platformImageRelevance(lowerKeyword, name) > 0 {
			out = append(out, img)
		}
	}
	return out
}

func firstImageMap(imageSet []any) map[string]any {
	for _, item := range imageSet {
		if img, _ := item.(map[string]any); img != nil {
			return img
		}
	}
	return nil
}

func firstUsableImageMap(imageSet []any) map[string]any {
	for _, item := range imageSet {
		if img, _ := item.(map[string]any); img != nil && platformImageStatusUsable(img) {
			return img
		}
	}
	return nil
}

func platformImageStatusUsable(img map[string]any) bool {
	status, _ := img["Status"].(string)
	status = strings.TrimSpace(status)
	return status == "" || status == deployment.ImageStatusAvailable
}

func createImageUnavailableError(params map[string]any) error {
	imageName := strings.TrimSpace(paramStr(params, "ImageName", ""))
	if imageName != "" {
		return fmt.Errorf("未找到可用的 %s 镜像；候选镜像可能已下线或不适配当前实例形态，请换镜像或稍后重试", imageName)
	}
	return fmt.Errorf("未找到可用镜像，无法创建实例；请换镜像或稍后重试")
}

func firstViablePlatformImage(params map[string]any, imageSet []any) map[string]any {
	maps := make([]map[string]any, 0, len(imageSet))
	for _, item := range imageSet {
		if img, _ := item.(map[string]any); img != nil {
			maps = append(maps, img)
		}
	}
	viable := viablePlatformImageIDs(params, maps)
	for _, img := range maps {
		if id, _ := img["CompShareImageId"].(string); viable[id] {
			return img
		}
	}
	return nil
}

func bestViablePlatformImage(params map[string]any, images []map[string]any) map[string]any {
	if len(images) == 0 {
		return nil
	}
	images, _ = platformImagesForIntent(params, images)
	candidates, byID := platformImageCandidates(images)
	selected := deployment.SelectImageCandidates(deployment.ImageSelectionInput{
		Images:       candidates,
		RequestedGPU: paramStr(params, "GpuType", ""),
		Zone: deployment.ZoneConstraint{
			Zone:  paramStr(params, "Zone", ""),
			IsPod: paramBool(params, "ZoneIsPod", false) || paramBool(params, "IsPodZone", false),
		},
	})
	if len(selected.Viable) == 0 {
		return nil
	}
	return byID[selected.Viable[0].Image.ID]
}

func platformImagesForIntent(params map[string]any, images []map[string]any) ([]map[string]any, bool) {
	keyword := strings.ToLower(strings.TrimSpace(paramStr(params, "ImageName", "")))
	if len(images) == 0 {
		return images, false
	}
	if keyword == "" {
		if purpose := strings.TrimSpace(paramStr(params, "ImagePurpose", "")); purpose != "" {
			return platformImagesForPurpose(images, normalizeImagePurpose(purpose))
		}
		return images, false
	}
	type rankedImage struct {
		img       map[string]any
		relevance int
		version   []int
		index     int
	}
	ranked := make([]rankedImage, 0, len(images))
	for i, img := range images {
		name, _ := img["Name"].(string)
		relevance := platformImageRelevance(keyword, name)
		if relevance <= 0 {
			continue
		}
		ranked = append(ranked, rankedImage{
			img:       img,
			relevance: relevance,
			version:   platformImageVersionKey(keyword, name),
			index:     i,
		})
	}
	if len(ranked) == 0 {
		return images, false
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].relevance != ranked[j].relevance {
			return ranked[i].relevance > ranked[j].relevance
		}
		if c := compareVersionKeys(ranked[i].version, ranked[j].version); c != 0 {
			return c > 0
		}
		return ranked[i].index < ranked[j].index
	})
	out := make([]map[string]any, 0, len(ranked))
	for _, item := range ranked {
		out = append(out, item.img)
	}
	return out, true
}

func platformImagesForPurpose(images []map[string]any, purpose string) ([]map[string]any, bool) {
	if normalizeImagePurpose(purpose) == imagePurposeCommunity {
		return nil, true
	}
	type rankedImage struct {
		img       map[string]any
		relevance int
		index     int
	}
	ranked := make([]rankedImage, 0, len(images))
	for i, img := range images {
		name, _ := img["Name"].(string)
		imageType, _ := img["ImageType"].(string)
		relevance := platformImagePurposeRelevance(purpose, name, imageType)
		if relevance <= 0 {
			continue
		}
		ranked = append(ranked, rankedImage{img: img, relevance: relevance, index: i})
	}
	if len(ranked) == 0 {
		return images, false
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].relevance != ranked[j].relevance {
			return ranked[i].relevance > ranked[j].relevance
		}
		return ranked[i].index < ranked[j].index
	})
	out := make([]map[string]any, 0, len(ranked))
	for _, item := range ranked {
		out = append(out, item.img)
	}
	return out, true
}

func platformImagePurposeRelevance(purpose, name, imageType string) int {
	nm := strings.ToLower(strings.TrimSpace(name))
	it := strings.ToLower(strings.TrimSpace(imageType))
	if nm == "" {
		return 0
	}
	containsAny := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(nm, strings.ToLower(w)) {
				return true
			}
		}
		return false
	}
	switch normalizeImagePurpose(purpose) {
	case imagePurposeDeepLearning:
		if containsAny("pytorch", "torch", "cuda", "tensorflow", "miniconda", "conda", "python") {
			return 100
		}
	case imagePurposeLLMInference:
		if containsAny("vllm", "ollama", "sglang") {
			return 100
		}
	case imagePurposeImageVideo:
		if containsAny("comfyui", "sd-webui", "stable diffusion", "图生视频", "图像", "视频", "wan") {
			return 100
		}
	case imagePurposeSystem:
		if strings.EqualFold(it, deployment.ImageTypeSystem) || containsAny("ubuntu", "windows") {
			return 100
		}
	case imagePurposePlatformApp:
		if strings.EqualFold(it, deployment.ImageTypeApp) || containsAny("dify", "ragflow", "docker", "isaac", "niugee") {
			return 100
		}
	}
	return 0
}

func platformImageRelevance(keyword, name string) int {
	kw := strings.ToLower(strings.TrimSpace(keyword))
	nm := strings.ToLower(strings.TrimSpace(name))
	if kw == "" || nm == "" {
		return 0
	}
	if nm == kw {
		return 300
	}
	if strings.Contains(nm, kw) {
		return 200
	}
	if (kw == "pytorch" || kw == "torch") && strings.Contains(nm, "torch") {
		return 180
	}
	if kw == "cuda" && strings.Contains(nm, "nvidia") {
		return 120
	}
	if sharesImageSubstring(kw, nm, 4) {
		return 100
	}
	return 0
}

var (
	imageNumberRE = regexp.MustCompile(`\d+(?:\.\d+)*`)
	torchTagRE    = regexp.MustCompile(`(?i)torch[_-]?(\d{2,4})`)
	cudaTagRE     = regexp.MustCompile(`(?i)cuda[_-]?(\d{2,4})`)
)

func platformImageVersionKey(keyword, name string) []int {
	lowerKeyword := strings.ToLower(strings.TrimSpace(keyword))
	lowerName := strings.ToLower(strings.TrimSpace(name))
	var key []int
	if lowerKeyword == "torch" || lowerKeyword == "pytorch" || strings.Contains(lowerKeyword, "torch") {
		if v := versionAfterToken(lowerName, "pytorch"); len(v) > 0 {
			key = append(key, v...)
		}
		if v := packedVersionFromRegex(torchTagRE, lowerName); len(v) > 0 {
			key = append(key, v...)
		}
		if v := packedVersionFromRegex(cudaTagRE, lowerName); len(v) > 0 {
			key = append(key, v...)
		}
		if len(key) > 0 {
			return key
		}
	}
	if lowerKeyword == "cuda" || strings.Contains(lowerKeyword, "cuda") {
		if v := packedVersionFromRegex(cudaTagRE, lowerName); len(v) > 0 {
			return v
		}
	}
	return firstNumericVersion(lowerName)
}

func versionAfterToken(name, token string) []int {
	idx := strings.Index(name, token)
	if idx < 0 {
		return nil
	}
	tail := name[idx+len(token):]
	match := imageNumberRE.FindString(tail)
	return parseVersionParts(match)
}

func packedVersionFromRegex(re *regexp.Regexp, name string) []int {
	m := re.FindStringSubmatch(name)
	if len(m) < 2 {
		return nil
	}
	s := m[1]
	if strings.Contains(s, ".") {
		return parseVersionParts(s)
	}
	switch len(s) {
	case 2:
		return []int{atoiZero(s[:1]), atoiZero(s[1:])}
	case 3:
		return []int{atoiZero(s[:1]), atoiZero(s[1:2]), atoiZero(s[2:])}
	case 4:
		return []int{atoiZero(s[:2]), atoiZero(s[2:3]), atoiZero(s[3:])}
	default:
		return []int{atoiZero(s)}
	}
}

func firstNumericVersion(name string) []int {
	return parseVersionParts(imageNumberRE.FindString(name))
}

func parseVersionParts(s string) []int {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	raw := strings.Split(s, ".")
	out := make([]int, 0, len(raw))
	for _, part := range raw {
		if part == "" {
			continue
		}
		out = append(out, atoiZero(part))
	}
	return out
}

func atoiZero(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func compareVersionKeys(a, b []int) int {
	max := len(a)
	if len(b) > max {
		max = len(b)
	}
	for i := 0; i < max; i++ {
		var av, bv int
		if i < len(a) {
			av = a[i]
		}
		if i < len(b) {
			bv = b[i]
		}
		if av != bv {
			return av - bv
		}
	}
	return 0
}

func sharesImageSubstring(a, b string, minLen int) bool {
	if len(a) < minLen || len(b) < minLen {
		return false
	}
	for i := 0; i+minLen <= len(a); i++ {
		if strings.Contains(b, a[i:i+minLen]) {
			return true
		}
	}
	return false
}

func viablePlatformImageIDs(params map[string]any, images []map[string]any) map[string]bool {
	candidates, _ := platformImageCandidates(images)
	selected := deployment.SelectImageCandidates(deployment.ImageSelectionInput{
		Images:       candidates,
		RequestedGPU: "",
		Zone: deployment.ZoneConstraint{
			Zone:  paramStr(params, "Zone", ""),
			IsPod: paramBool(params, "ZoneIsPod", false) || paramBool(params, "IsPodZone", false),
		},
	})
	out := make(map[string]bool, len(selected.Viable))
	for _, item := range selected.Viable {
		out[item.Image.ID] = true
	}
	return out
}

func platformImageCandidates(images []map[string]any) ([]deployment.ImageCandidate, map[string]map[string]any) {
	candidates := make([]deployment.ImageCandidate, 0, len(images))
	byID := make(map[string]map[string]any, len(images))
	for i, img := range images {
		id, _ := img["CompShareImageId"].(string)
		if id == "" {
			id = fmt.Sprintf("__image_%d", i)
		}
		name, _ := img["Name"].(string)
		imageType, _ := img["ImageType"].(string)
		status, _ := img["Status"].(string)
		candidates = append(candidates, deployment.ImageCandidate{
			ID:                id,
			Name:              name,
			ImageType:         imageType,
			Container:         paramBool(img, "Container", false) || paramBool(img, "IsContainer", false),
			Status:            status,
			SupportedGPUTypes: formStringSlice(img["SupportedGpuTypes"]),
		})
		byID[id] = img
	}
	return candidates, byID
}

// --- Community image helpers ---
// Community image response structure:
//   CompshareImageGroup[0].Data[0].CompShareImageId  // image ID
//   CompshareImageGroup[0].ImageName                  // group name
//   CompshareImageGroup[0].Data[0].Name               // version name

func pickFirstCommunityImageId(result map[string]any) string {
	if result == nil {
		return ""
	}
	groups, ok := result["CompshareImageGroup"].([]any)
	if !ok || len(groups) == 0 {
		return ""
	}
	group, ok := groups[0].(map[string]any)
	if !ok {
		return ""
	}
	data, ok := group["Data"].([]any)
	if !ok || len(data) == 0 {
		return ""
	}
	first, ok := data[0].(map[string]any)
	if !ok {
		return ""
	}
	if id, ok := first["CompShareImageId"].(string); ok {
		return id
	}
	return ""
}

func pickFirstCommunityImageName(result map[string]any) string {
	if result == nil {
		return "未知"
	}
	groups, ok := result["CompshareImageGroup"].([]any)
	if !ok || len(groups) == 0 {
		return "未知"
	}
	group, ok := groups[0].(map[string]any)
	if !ok {
		return "未知"
	}
	// Use group ImageName as the display name
	if name, ok := group["ImageName"].(string); ok && name != "" {
		return name
	}
	return "未知"
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
	maxFormGPUOptions                 = 5
	maxFormImageOptions               = 3
	maxGuidedImageOptions             = 10
	maxGuidedCommunityImageQueryLimit = 20
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

func createChargeTypeOptions(wfCtx *Context, zone string) []ConfirmFormOption {
	opts := make([]ConfirmFormOption, len(createFormChargeTypes))
	copy(opts, createFormChargeTypes)
	// Display-only: the Spot option is disabled for a pod zone. If the zone can't
	// be resolved here, show every charge type — the authoritative create gate
	// (validateCreatePlacement) still refuses an unresolvable or pod+Spot pick.
	placement, err := workflowZonePlacement(wfCtx, zone)
	if err != nil {
		return opts
	}
	for i := range opts {
		if !strings.EqualFold(opts[i].Value, deployment.ChargeTypeSpot) {
			continue
		}
		if placement.IsPod {
			opts[i].Disabled = true
			opts[i].Reason = "当前可用区不支持抢占式"
			opts[i].Note = "Pod 可用区暂不支持抢占式"
		}
	}
	return opts
}

const (
	imagePurposeDeepLearning = "deep_learning"
	imagePurposeLLMInference = "llm_inference"
	imagePurposeImageVideo   = "image_video"
	imagePurposeSystem       = "system"
	imagePurposePlatformApp  = "platform_app"
	imagePurposeCommunity    = "community"
	imagePurposeAppCommunity = "app_community" // legacy spelling; normalized to platform_app.
)

var createImagePurposeOptions = []ConfirmFormOption{
	{Value: imagePurposeDeepLearning, Label: "深度学习训练", Note: "PyTorch / CUDA / TensorFlow / Python 环境"},
	{Value: imagePurposeLLMInference, Label: "大模型推理", Note: "vLLM / Ollama / SGLang 等推理服务"},
	{Value: imagePurposeImageVideo, Label: "图像/视频生成", Note: "ComfyUI / SD-WebUI / 图生视频等应用"},
	{Value: imagePurposeSystem, Label: "普通系统", Note: "Ubuntu / Windows 系统镜像"},
	{Value: imagePurposePlatformApp, Label: "平台应用镜像", Note: "Dify / RAGFlow / Docker Compose / Isaac 等平台应用"},
	{Value: imagePurposeCommunity, Label: "社区镜像", Note: "来自社区镜像市场的真实镜像，默认展示使用较多的候选"},
}

func imagePurposeFormOptions() []ConfirmFormOption {
	opts := make([]ConfirmFormOption, len(createImagePurposeOptions))
	copy(opts, createImagePurposeOptions)
	return opts
}

func imagePurposeValue(params map[string]any) string {
	return normalizeImagePurpose(paramStr(params, "ImagePurpose", ""))
}

func normalizeImagePurpose(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case imagePurposeLLMInference:
		return imagePurposeLLMInference
	case imagePurposeImageVideo:
		return imagePurposeImageVideo
	case imagePurposeSystem:
		return imagePurposeSystem
	case imagePurposePlatformApp, imagePurposeAppCommunity:
		return imagePurposePlatformApp
	case imagePurposeCommunity:
		return imagePurposeCommunity
	default:
		return imagePurposeDeepLearning
	}
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
	images := wfCtx.Result("查询镜像")

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
	if cur, opts := imageFormOptions(wfCtx.Params, images, gpuType); cur != "" && len(opts) > 1 {
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

func guidedStepLabel(wfCtx *Context, logical int) string {
	index, total := guidedStepPosition(wfCtx, logical)
	return fmt.Sprintf("%d/%d", index, total)
}

func guidedStepPosition(wfCtx *Context, logical int) (int, int) {
	index, total := 0, 0
	for step := guidedStepGPU; step <= guidedStepFinal; step++ {
		visible := guidedStepReached(wfCtx, step) || step == logical || !guidedStepSkipped(wfCtx, step)
		if !visible {
			continue
		}
		total++
		if step == logical {
			index = total
		}
	}
	if index == 0 {
		index = total
	}
	if total == 0 {
		total = 1
	}
	return index, total
}

func guidedStepReached(wfCtx *Context, logical int) bool {
	if wfCtx == nil || wfCtx.Params == nil {
		return false
	}
	raw, ok := wfCtx.Params["GuidedReachedSteps"]
	if !ok || raw == nil {
		return false
	}
	key := strconv.Itoa(logical)
	switch steps := raw.(type) {
	case map[string]bool:
		return steps[key]
	case map[string]any:
		v, ok := steps[key]
		if !ok {
			return false
		}
		b, _ := v.(bool)
		return b
	default:
		return false
	}
}

func markGuidedStepReached(wfCtx *Context, logical int) {
	if wfCtx == nil || wfCtx.Params == nil {
		return
	}
	key := strconv.Itoa(logical)
	steps, _ := wfCtx.Params["GuidedReachedSteps"].(map[string]bool)
	if steps == nil {
		steps = map[string]bool{}
		wfCtx.Params["GuidedReachedSteps"] = steps
	}
	steps[key] = true
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
	case guidedStepImagePurpose:
		skip, err = shouldSkipGuidedImagePurposeStep(wfCtx)
	case guidedStepImage:
		skip, err = shouldSkipGuidedImageStep(wfCtx)
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

func guidedOrdinal(index int) string {
	switch index {
	case 1:
		return "第一步"
	case 2:
		return "第二步"
	case 3:
		return "第三步"
	case 4:
		return "第四步"
	case 5:
		return "第五步"
	default:
		return fmt.Sprintf("第%d步", index)
	}
}

func shouldSkipGuidedGPUStep(wfCtx *Context) (bool, error) {
	current := paramStr(wfCtx.Params, "GpuType", "")
	if current == "" || !paramBool(wfCtx.Params, "GuidedGpuLocked", false) {
		return false, nil
	}
	supported := currentImageSupportedGPUs(wfCtx.Params, wfCtx.Result("查询镜像"))
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
	selected, opts := guidedZoneFormOptions(wfCtx, wfCtx.Result("查询可用配比"), gpuType, current, wfCtx.Params, wfCtx.Result("查询GPU库存"))
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

func shouldSkipGuidedImagePurposeStep(wfCtx *Context) (bool, error) {
	return hasExplicitImageIntent(wfCtx.Params), nil
}

func shouldSkipGuidedImageStep(wfCtx *Context) (bool, error) {
	if strings.TrimSpace(paramStr(wfCtx.Params, "CompShareImageId", "")) != "" {
		return true, nil
	}
	if initialParamSet(wfCtx, "CompShareImageId") {
		return true, nil
	}
	if strings.TrimSpace(paramStr(wfCtx.InitialParams, "ImageName", "")) != "" {
		return true, nil
	}
	isCommunity := imagePurposeValue(wfCtx.Params) == imagePurposeCommunity ||
		strings.EqualFold(strings.TrimSpace(paramStr(wfCtx.Params, "ImageSource", "")), "community")
	if !isCommunity {
		return true, nil
	}
	return false, nil
}

func shouldSkipCommunityPurposeImageQuery(wfCtx *Context) (bool, error) {
	if imagePurposeValue(wfCtx.Params) != imagePurposeCommunity {
		return true, nil
	}
	if initialParamSet(wfCtx, "CompShareImageId") {
		return true, nil
	}
	if strings.EqualFold(strings.TrimSpace(paramStr(wfCtx.InitialParams, "ImageSource", "")), "community") {
		return true, nil
	}
	return false, nil
}

func initialParamSet(wfCtx *Context, key string) bool {
	if wfCtx == nil || wfCtx.InitialParams == nil {
		return false
	}
	_, ok := wfCtx.InitialParams[key]
	return ok
}

func enabledOptionCount(opts []ConfirmFormOption) (int, string) {
	count := 0
	only := ""
	for _, opt := range opts {
		if opt.Disabled {
			continue
		}
		count++
		only = opt.Value
	}
	return count, only
}

func enabledOptionExists(opts []ConfirmFormOption, value string) bool {
	for _, opt := range opts {
		if !opt.Disabled && strings.EqualFold(opt.Value, value) {
			return true
		}
	}
	return false
}

func containsString(list []string, needle string) bool {
	for _, item := range list {
		if item == needle {
			return true
		}
	}
	return false
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
	supported := currentImageSupportedGPUs(wfCtx.Params, wfCtx.Result("查询镜像"))
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
	_, opts := guidedZoneFormOptions(wfCtx, wfCtx.Result("查询可用配比"), gpuType, current, wfCtx.Params, wfCtx.Result("查询GPU库存"))
	if len(opts) == 0 {
		return nil, fmt.Errorf("%s 暂无可选可用区，请换一个 GPU 型号或稍后再试", gpuType)
	}
	index, total := guidedStepPosition(wfCtx, guidedStepZone)
	return &ConfirmForm{
		Version: 2,
		Step: &ConfirmFormStep{
			Index:          index,
			Total:          total,
			Title:          guidedStepTitle(index, "请选择可用区"),
			Description:    "可用区影响 GPU 现货与就近接入。建议优先选择有现货的可用区；同一型号在不同区的库存可能不同。",
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

func buildGuidedImagePurposeForm(wfCtx *Context) (*ConfirmForm, error) {
	current := imagePurposeValue(wfCtx.Params)
	index, total := guidedStepPosition(wfCtx, guidedStepImagePurpose)
	return &ConfirmForm{
		Version: 2,
		Step: &ConfirmFormStep{
			Index:          index,
			Total:          total,
			Title:          guidedStepTitle(index, "请选择主要用途"),
			Description:    "用途会影响推荐镜像范围。选择后只展示相关真实镜像，避免把系统镜像、框架镜像和应用镜像混在一起。",
			PrimaryLabel:   "确认选择",
			SecondaryLabel: "跳过",
			Skippable:      true,
		},
		Fields: []ConfirmFormField{{
			Key: "ImagePurpose", Label: "主要用途", Type: "select",
			Value: current, Render: "cards", Editable: true, Options: imagePurposeFormOptions(),
		}},
	}, nil
}

func buildGuidedImageForm(wfCtx *Context) (*ConfirmForm, error) {
	gpuType := paramStr(wfCtx.Params, "GpuType", "")
	current, opts := guidedImageFormOptions(wfCtx.Params, wfCtx.Result("查询镜像"), gpuType)
	if len(opts) == 0 {
		return nil, fmt.Errorf("未找到可选社区镜像，请换一个镜像来源或稍后再试")
	}
	if current == "" {
		current = opts[0].Value
	}
	index, total := guidedStepPosition(wfCtx, guidedStepImage)
	return &ConfirmForm{
		Version: 2,
		Step: &ConfirmFormStep{
			Index:          index,
			Total:          total,
			Title:          guidedStepTitle(index, "请选择社区镜像"),
			Description:    "不同社区镜像支持的 GPU 不同。置灰的镜像不支持当前卡型，需要更换镜像或 GPU 后才能创建。",
			PrimaryLabel:   "确认选择",
			SecondaryLabel: "取消",
		},
		Fields: []ConfirmFormField{{
			Key: "ImageId", Label: "社区镜像", Type: "select",
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
	images := wfCtx.Result("查询镜像")

	recommended := paramBool(wfCtx.Params, "GuidedRecommended", false)
	var fields []ConfirmFormField
	if cur, opts := guidedImageFormOptions(wfCtx.Params, images, gpuType); cur != "" && len(opts) > 0 {
		if recommended {
			markGuidedRecommendedOption(opts, cur)
		}
		fields = append(fields, ConfirmFormField{
			Key: "ImageId", Label: "镜像", Type: "select",
			Value: cur, Render: "cards", Editable: true, Options: opts,
		})
	}
	fields = append(fields, ConfirmFormField{
		Key: "ChargeType", Label: "计费方式", Type: "select",
		Value: createChargeType(wfCtx.Params), Render: "cards", Editable: true, Options: createChargeTypeOptions(wfCtx, paramStr(wfCtx.Params, "Zone", "")),
	})
	index, total := guidedStepPosition(wfCtx, guidedStepFinal)
	return &ConfirmForm{
		Version: 2,
		Step: &ConfirmFormStep{
			Index:          index,
			Total:          total,
			Title:          guidedStepTitle(index, "确认镜像与计费"),
			Description:    "镜像决定开机即用的预装环境（框架与驱动），计费方式可选按量或包时。确认无误后点击下方按钮即开始创建。",
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
			if name := imageNameByID(wfCtx.Result("查询镜像"), v); name != "" {
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
				if supported := currentImageSupportedGPUs(wfCtx.Params, wfCtx.Result("查询镜像")); len(supported) > 0 && !containsFold(supported, v) {
					delete(wfCtx.Params, "CompShareImageId")
					delete(wfCtx.Params, "ImageName")
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

func applyGuidedImagePurposeOverrides(wfCtx *Context, overrides map[string]string) error {
	for k, v := range overrides {
		switch k {
		case "ImagePurpose":
			old := paramStr(wfCtx.Params, "ImagePurpose", "")
			purpose := normalizeImagePurpose(v)
			wfCtx.Params["ImagePurpose"] = purpose
			if purpose == imagePurposeCommunity {
				wfCtx.Params["ImageSource"] = "community"
			} else {
				wfCtx.Params["ImageSource"] = "platform"
			}
			if !strings.EqualFold(old, v) {
				delete(wfCtx.Params, "CompShareImageId")
				delete(wfCtx.Params, "ImageName")
			}
		default:
			return fmt.Errorf("不支持修改字段 %s", k)
		}
	}
	markGuidedStepReached(wfCtx, guidedStepImagePurpose)
	return nil
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
	wfCtx.Params["ImageSource"] = "community"
	markGuidedStepReached(wfCtx, guidedStepImage)
	return nil
}

func ensureGuidedGPUType(wfCtx *Context) (string, error) {
	current, _ := wfCtx.Params["GpuType"].(string)
	supported := currentImageSupportedGPUs(wfCtx.Params, wfCtx.Result("查询镜像"))
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
	selected, opts := guidedZoneFormOptions(wfCtx, wfCtx.Result("查询可用配比"), gpuType, current, wfCtx.Params, wfCtx.Result("查询GPU库存"))
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
	counts map[string]map[string]float64
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
	poolName := "Exclusive"
	if strings.EqualFold(createChargeType(wfCtx.Params), "Spot") {
		poolName = "Spot"
	}
	rawPool, _ := rawInv[poolName].(map[string]any)
	if rawPool == nil {
		return guidedInventory{}
	}
	counts := map[string]map[string]float64{}
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
			counts[zone] = map[string]float64{}
		}
		for gpuType, rawCount := range gpuCounts {
			counts[zone][gpuType] = anyFloat(rawCount)
		}
	}
	if len(counts) == 0 {
		return guidedInventory{}
	}
	return guidedInventory{counts: counts}
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
	gpus, ok := inv.counts[zone]
	if !ok {
		return 0, false
	}
	return gpus[gpuType], true
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

func guidedStockNote(count float64) string {
	if count <= 0 {
		return "库存快照为 0，待确认"
	}
	return fmt.Sprintf("库存约 %.0f 张 GPU", count)
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
		disabled := !ch.normal || imageUnsupported
		disabledReason := ""
		if imageUnsupported {
			noteParts = append(noteParts, "镜像不支持当前 GPU")
			disabledReason = "镜像不支持当前 GPU"
		}
		if ch.normal {
			if stockKnown {
				noteParts = append(noteParts, guidedStockNote(stock))
			} else {
				noteParts = append(noteParts, "可售")
			}
		}
		if !ch.normal {
			noteParts = append(noteParts, "暂不可售")
			if disabledReason == "" {
				disabledReason = "暂不可售"
			}
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

func guidedZoneFormOptions(wfCtx *Context, catalog map[string]any, gpuType, current string, params map[string]any, inventoryResult map[string]any) (string, []ConfirmFormOption) {
	if catalog == nil || gpuType == "" {
		return "", nil
	}
	inventory := guidedInventoryFrom(wfCtx, inventoryResult)
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
		zone, _ := mt["Zone"].(string)
		if zone == "" {
			zone = defaultZone
		}
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
	selected := current
	if selected == "" || !seen[selected] || !enabledOptionExists(opts, selected) {
		selected = firstEnabledValue(opts)
	}
	return selected, opts
}

func guidedGPUCountFormOptions(wfCtx *Context, catalog map[string]any, gpuType, zone string, current float64, params map[string]any, inventoryResult map[string]any) (float64, []ConfirmFormOption) {
	if catalog == nil || gpuType == "" || zone == "" {
		return 0, nil
	}
	inventory := guidedInventoryFrom(wfCtx, inventoryResult)
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
			entryZone = defaultZone
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
			entryZone = defaultZone
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

func guidedImageFormOptions(params map[string]any, images map[string]any, gpuType string) (string, []ConfirmFormOption) {
	if images == nil {
		return "", nil
	}
	current := pickImageId(params, images)
	seen := map[string]bool{}
	var opts []ConfirmFormOption

	appendOpt := func(id, label string, warnings []string) {
		if id == "" || seen[id] || len(opts) >= maxGuidedImageOptions {
			return
		}
		seen[id] = true
		note := ""
		disabled := false
		reason := ""
		if containsString(warnings, deployment.WarningSupportedGPUMismatch) {
			note = "所选镜像不支持当前 GPU"
			reason = "镜像不支持当前 GPU"
			disabled = true
		}
		opts = append(opts, ConfirmFormOption{
			Value:    id,
			Label:    label,
			Note:     note,
			Reason:   reason,
			Disabled: disabled,
			Meta:     map[string]string{"ImageId": id},
		})
	}

	if current != "" {
		label := imageNameByID(images, current)
		if label == "" {
			label = pickImageName(params, images)
		}
		appendOpt(current, label, imageMismatchWarnings(gpuType, imageSupportedByID(images, current)))
	}
	if groups, ok := images["CompshareImageGroup"].([]any); ok {
		candidates, labels := communityImageCandidates(groups)
		selected := deployment.SelectImageCandidates(deployment.ImageSelectionInput{
			Images:       candidates,
			RequestedGPU: gpuType,
			Zone: deployment.ZoneConstraint{
				Zone:  paramStr(params, "Zone", ""),
				IsPod: paramBool(params, "ZoneIsPod", false) || paramBool(params, "IsPodZone", false),
			},
		})
		for _, viable := range selected.Viable {
			appendOpt(viable.Image.ID, labels[viable.Image.ID], viable.Warnings)
		}
	} else {
		var maps []map[string]any
		imageSet, _ := images["ImageSet"].([]any)
		for _, item := range imageSet {
			if img, _ := item.(map[string]any); img != nil {
				maps = append(maps, img)
			}
		}
		if ranked, narrowed := platformImagesForIntent(params, maps); narrowed {
			maps = ranked
		}
		candidates, byID := platformImageCandidates(maps)
		selected := deployment.SelectImageCandidates(deployment.ImageSelectionInput{
			Images:       candidates,
			RequestedGPU: gpuType,
			Zone: deployment.ZoneConstraint{
				Zone:  paramStr(params, "Zone", ""),
				IsPod: paramBool(params, "ZoneIsPod", false) || paramBool(params, "IsPodZone", false),
			},
		})
		for _, viable := range selected.Viable {
			img := byID[viable.Image.ID]
			if img == nil {
				continue
			}
			name, _ := img["Name"].(string)
			appendOpt(viable.Image.ID, name, viable.Warnings)
		}
	}
	if len(opts) == 0 {
		return "", nil
	}
	if !seen[current] || !enabledOptionExists(opts, current) {
		current = firstEnabledValue(opts)
	}
	return current, opts
}

func imageMismatchWarnings(gpuType string, supported []string) []string {
	if gpuType != "" && len(supported) > 0 && !containsFold(supported, gpuType) {
		return []string{deployment.WarningSupportedGPUMismatch}
	}
	return nil
}

func communityImageCandidates(groups []any) ([]deployment.ImageCandidate, map[string]string) {
	var candidates []deployment.ImageCandidate
	labels := map[string]string{}
	for i, g := range groups {
		gm, _ := g.(map[string]any)
		if gm == nil {
			continue
		}
		groupName, _ := gm["ImageName"].(string)
		data, _ := gm["Data"].([]any)
		for j, d := range data {
			dm, _ := d.(map[string]any)
			if dm == nil {
				continue
			}
			id, _ := dm["CompShareImageId"].(string)
			if id == "" {
				id = fmt.Sprintf("__community_%d_%d", i, j)
			}
			label := groupName
			if label == "" {
				label, _ = dm["Name"].(string)
			}
			status, _ := dm["Status"].(string)
			candidates = append(candidates, deployment.ImageCandidate{
				ID:                id,
				Name:              label,
				ImageType:         deployment.ImageTypeCommunity,
				Container:         paramBool(dm, "Container", false) || paramBool(dm, "IsContainer", false),
				Status:            status,
				SupportedGPUTypes: formStringSlice(dm["SupportedGpuTypes"]),
			})
			labels[id] = label
			break
		}
	}
	return candidates, labels
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
func imageFormOptions(params map[string]any, images map[string]any, gpuType string) (string, []ConfirmFormOption) {
	current := pickImageId(params, images)
	if current == "" || images == nil {
		return "", nil
	}
	opts := []ConfirmFormOption{}
	seen := map[string]bool{}
	zoneIsPod := paramBool(params, "ZoneIsPod", false) || paramBool(params, "IsPodZone", false)

	allowed := func(supported []string, container bool) bool {
		if zoneIsPod && !container {
			return false
		}
		if len(supported) > 0 && !containsFold(supported, gpuType) {
			return false
		}
		return true
	}
	appendOpt := func(id, label string, supported []string, container bool) {
		if id == "" || seen[id] || len(opts) >= maxFormImageOptions {
			return
		}
		if !allowed(supported, container) {
			return
		}
		seen[id] = true
		opts = append(opts, ConfirmFormOption{Value: id, Label: label})
	}
	if allowed(imageSupportedByID(images, current), imageContainerByID(images, current)) {
		currentLabel := imageNameByID(images, current)
		if currentLabel == "" {
			currentLabel = pickImageName(params, images)
		}
		appendOpt(current, currentLabel, imageSupportedByID(images, current), imageContainerByID(images, current))
	} else {
		current = ""
	}

	if groups, ok := images["CompshareImageGroup"].([]any); ok {
		for _, g := range groups {
			gm, _ := g.(map[string]any)
			if gm == nil {
				continue
			}
			data, _ := gm["Data"].([]any)
			if len(data) == 0 {
				continue
			}
			d0, _ := data[0].(map[string]any)
			id, _ := d0["CompShareImageId"].(string)
			name, _ := gm["ImageName"].(string)
			appendOpt(id, name, formStringSlice(d0["SupportedGpuTypes"]), paramBool(d0, "Container", false) || paramBool(d0, "IsContainer", false))
		}
		if current == "" && len(opts) > 0 {
			current = opts[0].Value
		}
		return current, opts
	}

	imageSet, _ := images["ImageSet"].([]any)
	maps := platformImageMaps(imageSet)
	if ranked, narrowed := platformImagesForIntent(params, maps); narrowed {
		maps = ranked
	}
	for _, img := range maps {
		id, _ := img["CompShareImageId"].(string)
		name, _ := img["Name"].(string)
		appendOpt(id, name, formStringSlice(img["SupportedGpuTypes"]), paramBool(img, "Container", false) || paramBool(img, "IsContainer", false))
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
