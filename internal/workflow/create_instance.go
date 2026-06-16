package workflow

import (
	"fmt"
	"strconv"
	"strings"
)

// defaultZone is the default availability zone per API docs (cn-wlcb-01, not cn-wlcb-a).
const defaultZone = "cn-wlcb-01"

const guidedCreateStepTotal = 5

// defaultDisk is the minimum required disk configuration for instance creation.
// The system disk has a 200GB free tier on CompShare.
//
// SYNC: this value is mirrored in engine.deployPrecheckDisk (deploy_model.go) so the
// deploy handler's pre-create per-zone stock check passes the same Disks the saga will.
// Keep the two in sync if this changes.
var defaultDisk = []any{
	map[string]any{"IsBoot": true, "Type": "CLOUD_SSD", "Size": 60},
}

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

// CreateInstanceDef returns the 7-step workflow definition for creating a
// CompShare GPU instance.
func CreateInstanceDef() *Definition {
	return &Definition{
		Name:        "CreateInstanceWorkflow",
		Description: "查询镜像 → 查询可用配比 → 检查库存 → 查询价格 → 确认 → 创建实例 → 查看状态",
		Steps: []Step{
			stepQueryImages(),
			stepQueryInstanceTypes(),
			stepCheckCapacity(),
			stepGetPrice(),
			stepConfirmCreate(),
			stepCreateInstance(),
			stepDescribeInstance(),
		},
	}
}

// CreateInstanceGuidedDef returns the guided, Figma-style order flow for
// creating a CompShare GPU instance. The public action name stays
// CreateInstanceWorkflow so old tooling and confirmation labels remain stable.
func CreateInstanceGuidedDef() *Definition {
	return &Definition{
		Name:        "CreateInstanceWorkflow",
		Description: "查询镜像 → 查询可用配比 → 选择 GPU → 选择可用区 → 选择卡数量 → 选择 CPU/内存 → 检查库存 → 查询价格 → 确认镜像计费 → 创建实例 → 查看状态",
		Steps: []Step{
			stepQueryImages(),
			stepQueryInstanceTypes(),
			stepGuidedChooseGPU(),
			stepGuidedChooseZone(),
			stepGuidedChooseGPUCount(),
			stepGuidedChooseCPUMemory(),
			stepCheckCapacity(),
			stepGetPrice(),
			stepConfirmCreateGuided(),
			stepCreateInstance(),
			stepDescribeInstance(),
		},
	}
}

// ---------------------------------------------------------------------------
// Step definitions (params aligned with docs/api/ specs)
// ---------------------------------------------------------------------------

func stepQueryImages() Step {
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
					return nil, fmt.Errorf("使用社区镜像创建实例时必须指定镜像名称（ImageName），请告诉我您想使用哪个社区镜像")
				}
				return map[string]any{"FuzzySearch": name}, nil
			}
			args := map[string]any{
				"Limit": 20,
			}
			if name := paramStr(wfCtx.Params, "ImageName", ""); name != "" {
				args["Name"] = name
			}
			return args, nil
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
				addZoneRegion(args, z)
			}
			return args, nil
		},
	}
}

func stepCheckCapacity() Step {
	return Step{
		Name: "检查库存",
		Type: StepToolCall,
		Tool: "CheckCompShareResourceCapacity",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			// Resolve target spec early so ambiguity errors surface before
			// making a pointless capacity API call. The resolved zone is where
			// the requested GPU actually lives (not a hardcoded default).
			_, _, _, zone, err := resolveTargetSpec(wfCtx)
			if err != nil {
				return nil, err
			}
			imageId := pickImageId(wfCtx.Params, wfCtx.Result("查询镜像"))
			return addZoneRegion(map[string]any{
				"Zone":               zone,
				"GpuType":            wfCtx.Params["GpuType"],
				"MachineType":        "G",
				"MinimalCpuPlatform": "Auto",
				"CompShareImageId":   imageId,
				"ChargeType":         createChargeType(wfCtx.Params),
				"Disks":              defaultDisk,
			}, zone), nil
		},
		CheckResult: func(wfCtx *Context, result map[string]any) (bool, string) {
			specs, _ := result["Specs"].([]any)
			if len(specs) == 0 {
				return false, "库存检查未返回任何规格信息，可能当前 GPU 型号不可用。"
			}

			gpu, cpu, memMB, _, err := resolveTargetSpec(wfCtx)
			if err != nil {
				return false, err.Error()
			}
			memGB := memMB / 1024 // Specs.Mem is in GB; our memoryMB is in MB

			// Match the exact GPU/CPU/Mem combination the workflow will create.
			for _, s := range specs {
				spec, _ := s.(map[string]any)
				sGpu, _ := spec["Gpu"].(float64)
				sCpu, _ := spec["Cpu"].(float64)
				sMem, _ := spec["Mem"].(float64)
				if sGpu == gpu && sCpu == cpu && sMem == memGB {
					if enough, _ := spec["ResourceEnough"].(bool); enough {
						return true, ""
					}
					gt, _ := wfCtx.Params["GpuType"].(string)
					if gt == "" {
						gt = "该 GPU"
					}
					return false, fmt.Sprintf("%s %.0f 卡 / %.0fC / %.0fGB 当前库存不足（售罄），请换一个规格或稍后再试。", gt, gpu, cpu, memGB)
				}
			}

			gt, _ := wfCtx.Params["GpuType"].(string)
			if gt == "" {
				gt = "该 GPU"
			}
			return false, fmt.Sprintf("库存中未找到 %s %.0f 卡 / %.0fC / %.0fGB 的规格组合，请确认配置是否正确。", gt, gpu, cpu, memGB)
		},
	}
}

func stepGetPrice() Step {
	return Step{
		Name: "查询价格",
		Type: StepToolCall,
		Tool: "GetCompShareInstanceUserPrice",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			gpu, cpu, mem, zone, err := resolveTargetSpec(wfCtx)
			if err != nil {
				return nil, err
			}
			gt, _ := wfCtx.Params["GpuType"].(string)
			args := map[string]any{
				"Zone":       zone,
				"GpuType":    gt,
				"GPU":        gpu,
				"CPU":        cpu,
				"Memory":     mem,
				"ChargeType": createChargeType(wfCtx.Params),
			}
			// Community images may be paid; include CompShareImageId for accurate pricing.
			// Prefer a threaded id (deploy_model handler) so price reflects the exact image
			// the saga will create, not an independently re-resolved one.
			if paramStr(wfCtx.Params, "ImageSource", "platform") == "community" {
				imageId := paramStr(wfCtx.Params, "CompShareImageId", "")
				if imageId == "" {
					imageId = pickFirstCommunityImageId(wfCtx.Result("查询镜像"))
				}
				if imageId != "" {
					args["CompShareImageId"] = imageId
				}
			}
			return addZoneRegion(args, zone), nil
		},
	}
}

func stepGuidedChooseGPU() Step {
	return Step{
		Name:              "选择 GPU",
		Type:              StepConfirm,
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
				"step":     "1/5",
				"GpuType":  gpuType,
			}, nil
		},
	}
}

func stepGuidedChooseZone() Step {
	return Step{
		Name:              "选择可用区",
		Type:              StepConfirm,
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
				"step":     "2/5",
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
				"step":     "3/5",
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
			current, opts := guidedCpuMemoryFormOptions(wfCtx.Result("查询可用配比"), gpuType, zone, gpu, wfCtx.Params)
			if current == "" || len(opts) == 0 {
				return nil, fmt.Errorf("%s 在 %s 的 %.0f 卡暂无可选 CPU/内存规格，请换一个可用区或卡数量", gpuType, zone, gpu)
			}
			return map[string]any{
				"workflow":  "CreateInstanceWorkflow",
				"step":      "4/5",
				"GpuType":   gpuType,
				"Zone":      zone,
				"Gpu":       gpu,
				"CpuMemory": current,
			}, nil
		},
	}
}

// createChargeType normalizes the create path to the current upstream billing
// contract: pay-as-you-go/hourly uses Postpay. Dynamic is a deprecated input
// spelling kept only for backward compatibility with older LLM/tool args.
func createChargeType(params map[string]any) string {
	ct := paramStr(params, "ChargeType", "")
	if ct == "" || ct == "Dynamic" {
		return "Postpay"
	}
	return ct
}

// confirmPriceText renders a human-readable hourly/period price for the confirm
// card from the GetCompShareInstanceUserPrice result, instead of putting the raw
// API object into the card (the frontend stringified that as "[object Object]").
// It reads the payable amount for the resolved ChargeType out of PriceDetails,
// appending the list price as 原价 when a discount applies. Returns "" when the
// result is missing/empty/an unexpected shape so the caller omits the price line
// rather than surfacing a raw object. The UserPrice PriceDetails entry carries a
// "Price" field (per the API + create/golden/deploy_model fixtures); "Instance"
// is accepted as a fallback for robustness (it is the catalog-API field name).
func confirmPriceText(priceResult any, chargeType string) string {
	raw, ok := priceResult.(map[string]any)
	if !ok {
		return ""
	}
	amountFor := func(arrKey string) (float64, bool) {
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
			if n, ok := priceNumber(m["Price"]); ok {
				return n, true
			}
			if n, ok := priceNumber(m["Instance"]); ok {
				return n, true
			}
		}
		return 0, false
	}
	payable, ok := amountFor("PriceDetails")
	if !ok {
		return ""
	}
	text := fmt.Sprintf("¥%.2f%s", payable, chargePeriodUnit(chargeType))
	list, hasList := amountFor("ListPriceDetails")
	if !hasList {
		list, hasList = amountFor("OriginalPriceDetails")
	}
	if hasList && list > payable {
		text += fmt.Sprintf("（原价 ¥%.2f）", list)
	}
	return text
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
		BuildForm:       buildCreateConfirmForm,
		ApplyOverrides:  applyCreateOverrides,
		RevalidateSteps: []string{"检查库存", "查询价格"},
		BuildArgs:       buildCreateConfirmArgs,
	}
}

func stepConfirmCreateGuided() Step {
	return Step{
		Name:            "确认创建",
		Type:            StepConfirm,
		BuildForm:       buildGuidedFinalForm,
		ApplyOverrides:  applyCreateOverrides,
		RevalidateSteps: []string{"检查库存", "查询价格"},
		BuildArgs:       buildCreateConfirmArgs,
	}
}

func buildCreateConfirmArgs(wfCtx *Context) (map[string]any, error) {
	gpu, cpu, memMB, zone, err := resolveTargetSpec(wfCtx)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"workflow":   "CreateInstanceWorkflow",
		"GpuType":    wfCtx.Params["GpuType"],
		"Gpu":        gpu,
		"CPU":        cpu,
		"Memory":     memMB,
		"Zone":       zone,
		"ChargeType": createChargeType(wfCtx.Params),
		"image":      pickImageName(wfCtx.Params, wfCtx.Result("查询镜像")),
		"price":      confirmPriceText(wfCtx.Result("查询价格"), createChargeType(wfCtx.Params)),
		// FallbackNote is set by the deploy_model handler when it switched the
		// create-zone (sold-out primary). Empty for the CLI/ReAct create path.
		// Surfaced in the confirm card so the user sees the zone switch before
		// approving. The key is always present (value "" when unset); the
		// renderer (cli.go printCreateConfirmCard) skips it when empty.
		"FallbackNote": paramStr(wfCtx.Params, "FallbackNote", ""),
	}, nil
}

func stepCreateInstance() Step {
	return Step{
		Name: "创建实例",
		Type: StepToolCall,
		Tool: "CreateCompShareInstance",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			gpu, cpu, mem, zone, err := resolveTargetSpec(wfCtx)
			if err != nil {
				return nil, err
			}
			imageId := pickImageId(wfCtx.Params, wfCtx.Result("查询镜像"))
			if paramStr(wfCtx.Params, "ImageSource", "platform") == "community" && imageId == "" {
				// Fail loud rather than POST an empty CompShareImageId (which the
				// upstream rejects cryptically). The community path is the real risk:
				// pickFirstCommunityImageId returns "" when the group has no Data[]
				// array (a valid API shape). Scoped to community to leave the shipped
				// platform create path byte-identical (B8.3 review).
				return nil, fmt.Errorf("社区镜像未返回有效的镜像 ID，无法创建实例（请确认社区镜像名称是否正确）")
			}
			gt, _ := wfCtx.Params["GpuType"].(string)
			args := map[string]any{
				"Zone":             zone,
				"GpuType":          gt,
				"GPU":              gpu,
				"CPU":              cpu,
				"Memory":           mem,
				"CompShareImageId": imageId,
				"ChargeType":       createChargeType(wfCtx.Params),
				"Disks":            defaultDisk,
			}
			if name, ok := wfCtx.Params["Name"]; ok {
				args["Name"] = name
			}
			return addZoneRegion(args, zone), nil
		},
	}
}

func stepDescribeInstance() Step {
	return Step{
		Name: "查看状态",
		Type: StepToolCall,
		Tool: "DescribeCompShareInstance",
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
// Priority: exact name match (case-insensitive) > contains match > first entry.
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
		// No name preference — return first (backward-compatible)
		first, _ := imageSet[0].(map[string]any)
		return first
	}

	// Pass 1: case-insensitive exact match
	for _, item := range imageSet {
		img, _ := item.(map[string]any)
		name, _ := img["Name"].(string)
		if strings.EqualFold(name, keyword) {
			return img
		}
	}

	// Pass 2: case-insensitive contains match
	lowerKeyword := strings.ToLower(keyword)
	for _, item := range imageSet {
		img, _ := item.(map[string]any)
		name, _ := img["Name"].(string)
		if strings.Contains(strings.ToLower(name), lowerKeyword) {
			return img
		}
	}

	// No match — fall back to first entry
	first, _ := imageSet[0].(map[string]any)
	return first
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
	maxFormGPUOptions   = 5
	maxFormImageOptions = 3
)

// createFormChargeTypes are the selectable billing modes. Postpay is the
// platform default; the deprecated Dynamic spelling is normalized away by
// createChargeType and never offered.
var createFormChargeTypes = []ConfirmFormOption{
	{Value: "Postpay", Label: "按量付费（按小时计费）"},
	{Value: "Day", Label: "包日"},
	{Value: "Month", Label: "包月"},
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
	zoneDescribes, _ := wfCtx.Params["ZoneDescribes"].(map[string]string)
	if opts := zoneFormOptions(catalog, gpuType, zone, zoneDescribes); len(opts) > 1 {
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
		Value: createChargeType(wfCtx.Params), Editable: true, Options: createFormChargeTypes,
	})
	return &ConfirmForm{Version: 1, Fields: fields}, nil
}

func buildGuidedGPUForm(wfCtx *Context) (*ConfirmForm, error) {
	gpuType, err := ensureGuidedGPUType(wfCtx)
	if err != nil {
		return nil, err
	}
	supported := currentImageSupportedGPUs(wfCtx.Params, wfCtx.Result("查询镜像"))
	locked := paramBool(wfCtx.Params, "GuidedGpuLocked", false) && gpuType != ""
	recommended := paramBool(wfCtx.Params, "GuidedRecommended", false) && gpuType != ""
	_, opts := guidedGPUFormOptions(wfCtx.Result("查询可用配比"), supported, gpuType, locked)
	if len(opts) == 0 {
		return nil, fmt.Errorf("暂无可选 GPU 型号")
	}
	title := "第一步，请选择 GPU 参数"
	desc := "GPU 型号决定可用显存与算力：显存越大，越能支撑更大的模型与更高的批量。不确定时可先用默认项。"
	if locked {
		title = "第一步，请确认 GPU 参数"
		desc = "已按你的需求推荐合适的 GPU 显存规格，可直接确认，也可在下方调整。显存越大，可支撑的模型与批量越大。"
	} else if recommended {
		// A model-driven deploy keeps every GPU on the card but pre-selects the
		// matcher's pick; flag it so the recommendation is visible, not just default.
		title = "第一步，请确认推荐的 GPU 参数"
		desc = "已根据你要部署的模型推荐合适的 GPU 显存规格（默认已选中），如需更大显存可在下方调整。"
		markGuidedRecommendedOption(opts, gpuType)
	}
	return &ConfirmForm{
		Version: 2,
		Step: &ConfirmFormStep{
			Index:          1,
			Total:          guidedCreateStepTotal,
			Title:          title,
			Description:    desc,
			PrimaryLabel:   "确认选择",
			SecondaryLabel: "跳过",
			Skippable:      true,
		},
		Fields: []ConfirmFormField{{
			Key: "GpuType", Label: "GPU 参数", Type: "select",
			Value: gpuType, Render: "cards", Editable: true, Options: opts,
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
	_, opts := guidedZoneFormOptions(wfCtx.Result("查询可用配比"), gpuType, current)
	if len(opts) == 0 {
		return nil, fmt.Errorf("%s 暂无可选可用区，请换一个 GPU 型号或稍后再试", gpuType)
	}
	return &ConfirmForm{
		Version: 2,
		Step: &ConfirmFormStep{
			Index:          2,
			Total:          guidedCreateStepTotal,
			Title:          "第二步，请选择可用区",
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
	_, opts := guidedGPUCountFormOptions(wfCtx.Result("查询可用配比"), gpuType, zone, gpu)
	if len(opts) == 0 {
		return nil, fmt.Errorf("%s 在 %s 暂无可选卡数量，请换一个可用区", gpuType, zone)
	}
	current := fmt.Sprintf("%.0f", gpu)
	return &ConfirmForm{
		Version: 2,
		Step: &ConfirmFormStep{
			Index:          3,
			Total:          guidedCreateStepTotal,
			Title:          "第三步，请选择卡数量",
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
	_, opts := guidedCpuMemoryFormOptions(wfCtx.Result("查询可用配比"), gpuType, zone, gpu, wfCtx.Params)
	if len(opts) == 0 {
		return nil, fmt.Errorf("%s 在 %s 的 %.0f 卡暂无可选 CPU/内存规格，请换一个可用区或卡数量", gpuType, zone, gpu)
	}
	return &ConfirmForm{
		Version: 2,
		Step: &ConfirmFormStep{
			Index:          4,
			Total:          guidedCreateStepTotal,
			Title:          "第四步，请选择 CPU/内存",
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
	chargeOpts := make([]ConfirmFormOption, len(createFormChargeTypes))
	copy(chargeOpts, createFormChargeTypes)
	fields = append(fields, ConfirmFormField{
		Key: "ChargeType", Label: "计费方式", Type: "select",
		Value: createChargeType(wfCtx.Params), Render: "cards", Editable: true, Options: chargeOpts,
	})
	return &ConfirmForm{
		Version: 2,
		Step: &ConfirmFormStep{
			Index:          5,
			Total:          guidedCreateStepTotal,
			Title:          "第五步，确认镜像与计费",
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
			wfCtx.Params["GpuType"] = v
			delete(wfCtx.Params, "Gpu")
			delete(wfCtx.Params, "Cpu")
			delete(wfCtx.Params, "Memory")
			delete(wfCtx.Params, "Zone")
			if supported := currentImageSupportedGPUs(wfCtx.Params, wfCtx.Result("查询镜像")); len(supported) > 0 && !containsFold(supported, v) {
				delete(wfCtx.Params, "CompShareImageId")
				delete(wfCtx.Params, "ImageName")
			}
		default:
			return fmt.Errorf("不支持修改字段 %s", k)
		}
	}
	return nil
}

func applyGuidedZoneOverrides(wfCtx *Context, overrides map[string]string) error {
	for k, v := range overrides {
		switch k {
		case "Zone":
			wfCtx.Params["Zone"] = v
			delete(wfCtx.Params, "Gpu")
			delete(wfCtx.Params, "Cpu")
			delete(wfCtx.Params, "Memory")
		default:
			return fmt.Errorf("不支持修改字段 %s", k)
		}
	}
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
			wfCtx.Params["Gpu"] = gpu
			delete(wfCtx.Params, "Cpu")
			delete(wfCtx.Params, "Memory")
		default:
			return fmt.Errorf("不支持修改字段 %s", k)
		}
	}
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
			wfCtx.Params["Gpu"] = gpu
			wfCtx.Params["Cpu"] = cpu
			wfCtx.Params["Memory"] = memoryMB
		default:
			return fmt.Errorf("不支持修改字段 %s", k)
		}
	}
	return nil
}

func ensureGuidedGPUType(wfCtx *Context) (string, error) {
	current, _ := wfCtx.Params["GpuType"].(string)
	supported := currentImageSupportedGPUs(wfCtx.Params, wfCtx.Result("查询镜像"))
	locked := paramBool(wfCtx.Params, "GuidedGpuLocked", false) && current != ""
	selected, opts := guidedGPUFormOptions(wfCtx.Result("查询可用配比"), supported, current, locked)
	if selected == "" {
		for _, opt := range opts {
			if !opt.Disabled {
				selected = opt.Value
				break
			}
		}
	}
	if selected == "" {
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
	selected, opts := guidedZoneFormOptions(wfCtx.Result("查询可用配比"), gpuType, current)
	if selected == "" || len(opts) == 0 {
		return "", fmt.Errorf("%s 暂无可选可用区，请换一个 GPU 型号或稍后再试", gpuType)
	}
	wfCtx.Params["Zone"] = selected
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
	selected, opts := guidedGPUCountFormOptions(wfCtx.Result("查询可用配比"), gpuType, zone, current)
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
	current, opts := guidedCpuMemoryFormOptions(wfCtx.Result("查询可用配比"), gpuType, zone, gpu, wfCtx.Params)
	if current == "" || len(opts) == 0 {
		return "", fmt.Errorf("%s 在 %s 的 %.0f 卡暂无可选 CPU/内存规格，请换一个可用区或卡数量", gpuType, zone, gpu)
	}
	parsedZone, parsedGPU, cpu, memoryMB, err := parseGuidedSpecKey(current)
	if err != nil {
		return "", err
	}
	wfCtx.Params["Zone"] = parsedZone
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

func guidedGPUFormOptions(catalog map[string]any, supported []string, current string, locked bool) (string, []ConfirmFormOption) {
	if catalog == nil {
		return current, nil
	}
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
		if locked && current != "" && !guidedGPUIntentMatches(current, name) {
			continue
		}
		if !locked && len(supported) > 0 && !containsFold(supported, name) {
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
		if ch.vramGB > 0 {
			noteParts = append(noteParts, fmt.Sprintf("%.0fG 显存", ch.vramGB))
		}
		if ch.normal {
			noteParts = append(noteParts, "可售")
		} else {
			noteParts = append(noteParts, "暂不可售")
		}
		if len(ch.zones) > 0 {
			noteParts = append(noteParts, "可用区 "+strings.Join(ch.zones, "、"))
		}
		opt := ConfirmFormOption{
			Value:    ch.name,
			Label:    label,
			Note:     strings.Join(noteParts, " · "),
			Disabled: !ch.normal,
			Meta: map[string]string{
				"Sellable": strconv.FormatBool(ch.normal),
			},
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
	if selected == "" {
		for _, opt := range opts {
			if !opt.Disabled {
				selected = opt.Value
				break
			}
		}
	}
	return selected, opts
}

func guidedGPUIntentMatches(intent, candidate string) bool {
	if strings.EqualFold(intent, candidate) {
		return true
	}
	// A broad model mention like "4090" should include closely related variants
	// such as "4090_48G". A precise variant mention stays exact.
	if strings.ContainsAny(intent, "_-") {
		return false
	}
	intentLower := strings.ToLower(intent)
	candidateLower := strings.ToLower(candidate)
	return strings.HasPrefix(candidateLower, intentLower+"_") ||
		strings.HasPrefix(candidateLower, intentLower+"-")
}

func guidedZoneFormOptions(catalog map[string]any, gpuType, current string) (string, []ConfirmFormOption) {
	if catalog == nil || gpuType == "" {
		return "", nil
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
		zone, _ := mt["Zone"].(string)
		if zone == "" {
			zone = defaultZone
		}
		if seen[zone] {
			continue
		}
		seen[zone] = true
		opt := ConfirmFormOption{
			Value: zone,
			Label: zone,
			Note:  fmt.Sprintf("%s 可用", gpuType),
			Meta:  map[string]string{"Zone": zone},
		}
		if zone == current {
			opts = append([]ConfirmFormOption{opt}, opts...)
		} else {
			opts = append(opts, opt)
		}
	}
	selected := current
	if selected == "" || !seen[selected] {
		if len(opts) > 0 {
			selected = opts[0].Value
		}
	}
	return selected, opts
}

func guidedGPUCountFormOptions(catalog map[string]any, gpuType, zone string, current float64) (float64, []ConfirmFormOption) {
	if catalog == nil || gpuType == "" || zone == "" {
		return 0, nil
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
			if gpu <= 0 {
				continue
			}
			value := fmt.Sprintf("%.0f", gpu)
			if seen[value] {
				continue
			}
			seen[value] = true
			opt := ConfirmFormOption{
				Value: value,
				Label: fmt.Sprintf("%.0f 卡", gpu),
				Note:  fmt.Sprintf("%s · %s", gpuType, zone),
				Meta:  map[string]string{"GPU": value, "Zone": zone},
			}
			if current == gpu {
				opts = append([]ConfirmFormOption{opt}, opts...)
			} else {
				opts = append(opts, opt)
			}
		}
	}
	selected := current
	if selected == 0 || !seen[fmt.Sprintf("%.0f", selected)] {
		if len(opts) > 0 {
			selected, _ = strconv.ParseFloat(opts[0].Value, 64)
		}
	}
	return selected, opts
}

func guidedSpecFormOptions(catalog map[string]any, gpuType string, params map[string]any) (string, []ConfirmFormOption) {
	if catalog == nil || gpuType == "" {
		return "", nil
	}
	current := ""
	if z, ok := params["Zone"].(string); ok && z != "" {
		gpu := paramNum(params, "Gpu", 1)
		if _, hasCPU := params["Cpu"]; hasCPU {
			if _, hasMem := params["Memory"]; hasMem {
				current = formatGuidedSpecKey(z, gpu, paramNum(params, "Cpu", 0), paramNum(params, "Memory", 0))
			}
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
		zone, _ := mt["Zone"].(string)
		if zone == "" {
			zone = defaultZone
		}
		sizes, _ := mt["MachineSizes"].([]any)
		for _, s := range sizes {
			size, _ := s.(map[string]any)
			gpu, _ := size["Gpu"].(float64)
			if gpu == 0 {
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
					opts = append(opts, ConfirmFormOption{
						Value: key,
						Label: fmt.Sprintf("%s · %.0f 卡 · %.0f 核 CPU · %.0fGB 内存", zone, gpu, cpu, memGB),
						Note:  fmt.Sprintf("可用区 %s", zone),
						Meta: map[string]string{
							"Zone":     zone,
							"GPU":      fmt.Sprintf("%.0f", gpu),
							"CPU":      fmt.Sprintf("%.0f", cpu),
							"MemoryGB": fmt.Sprintf("%.0f", memGB),
						},
					})
				}
			}
		}
	}
	return current, opts
}

func guidedCpuMemoryFormOptions(catalog map[string]any, gpuType, zone string, gpuCount float64, params map[string]any) (string, []ConfirmFormOption) {
	if catalog == nil || gpuType == "" || zone == "" || gpuCount <= 0 {
		return "", nil
	}
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
					opts = append(opts, ConfirmFormOption{
						Value: key,
						Label: fmt.Sprintf("%.0f 核 CPU · %.0fGB 内存", cpu, memGB),
						Note:  fmt.Sprintf("%s · %.0f 卡 · %s", gpuType, gpu, zone),
						Meta: map[string]string{
							"Zone":     zone,
							"GPU":      fmt.Sprintf("%.0f", gpu),
							"CPU":      fmt.Sprintf("%.0f", cpu),
							"MemoryGB": fmt.Sprintf("%.0f", memGB),
						},
					})
				}
			}
		}
	}
	if current != "" && !seen[current] && len(opts) > 0 {
		current = opts[0].Value
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

	appendOpt := func(id, label string, supported []string, enforceSupport bool) {
		if id == "" || seen[id] {
			return
		}
		if enforceSupport && len(supported) > 0 && !containsFold(supported, gpuType) {
			return
		}
		seen[id] = true
		opts = append(opts, ConfirmFormOption{
			Value: id,
			Label: label,
			Meta:  map[string]string{"ImageId": id},
		})
	}

	if current != "" {
		label := imageNameByID(images, current)
		if label == "" {
			label = pickImageName(params, images)
		}
		appendOpt(current, label, imageSupportedByID(images, current), false)
	}
	if groups, ok := images["CompshareImageGroup"].([]any); ok {
		for _, g := range groups {
			gm, _ := g.(map[string]any)
			if gm == nil {
				continue
			}
			name, _ := gm["ImageName"].(string)
			data, _ := gm["Data"].([]any)
			for _, d := range data {
				dm, _ := d.(map[string]any)
				id, _ := dm["CompShareImageId"].(string)
				label := name
				if label == "" {
					label, _ = dm["Name"].(string)
				}
				appendOpt(id, label, formStringSlice(dm["SupportedGpuTypes"]), true)
			}
		}
	} else {
		imageSet, _ := images["ImageSet"].([]any)
		for _, item := range imageSet {
			img, _ := item.(map[string]any)
			if img == nil {
				continue
			}
			id, _ := img["CompShareImageId"].(string)
			name, _ := img["Name"].(string)
			appendOpt(id, name, formStringSlice(img["SupportedGpuTypes"]), true)
		}
	}
	if len(opts) == 0 {
		return "", nil
	}
	if !seen[current] {
		current = opts[0].Value
	}
	return current, opts
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

// zoneFormOptions lists the zones the current GPU type is sellable in,
// current zone first. describes (zone-id → 显示名, from the live support-zone
// catalog, threaded in via params["ZoneDescribes"]) labels each option with the
// console's Chinese name ("华北一C") so the user recognizes the zone; it falls
// back to the bare zone id when the name is unknown (CLI / catalog unavailable).
func zoneFormOptions(catalog map[string]any, gpuType, current string, describes map[string]string) []ConfirmFormOption {
	if catalog == nil {
		return nil
	}
	opts := []ConfirmFormOption{{Value: current, Label: zoneOptionLabel(describes, current)}}
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
		opts = append(opts, ConfirmFormOption{Value: z, Label: zoneOptionLabel(describes, z)})
	}
	return opts
}

// zoneOptionLabel renders a zone for the form: "华北一C (cn-bj2-03)" when the
// display name is known, else the bare zone id.
func zoneOptionLabel(describes map[string]string, zone string) string {
	if d := describes[zone]; d != "" {
		return d + " (" + zone + ")"
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
	currentLabel := imageNameByID(images, current)
	if currentLabel == "" {
		currentLabel = pickImageName(params, images)
	}
	opts := []ConfirmFormOption{{Value: current, Label: currentLabel}}
	seen := map[string]bool{current: true}

	appendOpt := func(id, label string, supported []string) {
		if id == "" || seen[id] || len(opts) >= maxFormImageOptions {
			return
		}
		if len(supported) > 0 && !containsFold(supported, gpuType) {
			return
		}
		seen[id] = true
		opts = append(opts, ConfirmFormOption{Value: id, Label: label})
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
			appendOpt(id, name, formStringSlice(d0["SupportedGpuTypes"]))
		}
		return current, opts
	}

	imageSet, _ := images["ImageSet"].([]any)
	for _, item := range imageSet {
		img, _ := item.(map[string]any)
		if img == nil {
			continue
		}
		id, _ := img["CompShareImageId"].(string)
		name, _ := img["Name"].(string)
		appendOpt(id, name, formStringSlice(img["SupportedGpuTypes"]))
	}
	return current, opts
}

// currentImageSupportedGPUs returns the SupportedGpuTypes declared by the
// currently selected image (empty = no constraint declared).
func currentImageSupportedGPUs(params map[string]any, images map[string]any) []string {
	if images == nil {
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
