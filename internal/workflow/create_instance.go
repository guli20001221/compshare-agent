package workflow

import (
	"fmt"
	"strings"
)

// defaultZone is the default availability zone per API docs (cn-wlcb-01, not cn-wlcb-a).
const defaultZone = "cn-wlcb-01"

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
			return map[string]any{
				"Zone":               zone,
				"GpuType":            wfCtx.Params["GpuType"],
				"MachineType":        "G",
				"MinimalCpuPlatform": "Auto",
				"CompShareImageId":   imageId,
				"ChargeType":         createChargeType(wfCtx.Params),
				"Disks":              defaultDisk,
			}, nil
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
			return args, nil
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

func stepConfirmCreate() Step {
	return Step{
		Name: "确认创建",
		Type: StepConfirm,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
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
				"price":      wfCtx.Result("查询价格"),
				// FallbackNote is set by the deploy_model handler when it switched the
				// create-zone (sold-out primary). Empty for the CLI/ReAct create path.
				// Surfaced in the confirm card so the user sees the zone switch before
				// approving. The key is always present (value "" when unset); the
				// renderer (cli.go printCreateConfirmCard) skips it when empty.
				"FallbackNote": paramStr(wfCtx.Params, "FallbackNote", ""),
			}, nil
		},
	}
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
			return args, nil
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
