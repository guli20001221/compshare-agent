package knowledge

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
)

// model_specs.go implements GetModelVRAMRequirement — the deterministic
// model-name → VRAM → GPU-fit tool (B8.1). Per the lead's design split
// (2026-05-30): the TOOL computes (parse param count, estimate VRAM, find a
// card that physically fits); the deploy_model SKILL judges (clarify an
// ambiguous model name, weigh quantization/buffer tradeoffs, frame perf/cost
// via GetGPURecommendation). This function is therefore pure arithmetic over
// the canonical gpuSpecs table — no LLM, no scene heuristics.

// bytesPerParam maps a quantization label to bytes-per-weight. FP16/BF16 store
// 2 bytes; INT8/FP8 1 byte; INT4 0.5 byte. Default (and most conservative for a
// "will it fit" check) is fp16.
var bytesPerParam = map[string]float64{
	"fp16": 2.0,
	"bf16": 2.0,
	"fp8":  1.0,
	"int8": 1.0,
	"int4": 0.5,
}

// vramBufferFactor pads the raw weight footprint to cover KV-cache, activations
// and framework overhead. ADR-003 deploy_model skill note: "VRAM 估算保守,加
// 20-30% buffer". 1.2 is the conservative single-request floor; long context or
// high concurrency needs more (surfaced in the note, weighed by the skill).
const vramBufferFactor = 1.2

// paramCountRE matches a parameter count embedded in a model name: an integer
// or decimal immediately followed by 'b'/'B' (billions), e.g. "32b", "70B",
// "1.5b", "Qwen32B", "Llama3-70B", "deepseek-67b". The leading boundary keeps
// "3b" in "fp3b8" from matching while allowing "qwen3-32b" → 32 (the LAST such
// token wins, see parseParamCountB).
var paramCountRE = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*b\b`)

// canonicalModels covers well-known models whose parameter count is NOT in the
// user-typed name (or is easy to mistype). Keys are normalized (lowercased,
// spaces/underscores stripped). Values are TOTAL parameter count in billions —
// for MoE models the total (all experts) is correct because every expert must
// be resident in VRAM even though only some are active per token. Kept small +
// public-spec-sourced; the regex path handles the long tail.
var canonicalModels = map[string]float64{
	"deepseekv3":   671, // MoE total
	"deepseekr1":   671, // MoE total
	"deepseekv2":   236, // MoE total
	"mixtral8x7b":  47,  // MoE total (~46.7B); regex would wrongly read "7b"
	"mixtral8x22b": 141, // MoE total (~141B)
	"qwq":          32,  // QwQ-32B (single variant; useful when size omitted)
}

// GetModelVRAMRequirement estimates the minimum VRAM a model needs and finds
// CompShare GPU configurations that physically fit it. quantization defaults to
// "fp16" when empty/unknown. The result is a JSON-serializable map; resolved
// is false (with guidance) when the parameter count cannot be determined — the
// skill then asks the user to confirm the model / parameter count.
func GetModelVRAMRequirement(modelName, quantization string) map[string]any {
	quant := strings.ToLower(strings.TrimSpace(quantization))
	bpp, ok := bytesPerParam[quant]
	if !ok {
		quant = "fp16"
		bpp = bytesPerParam[quant]
	}

	paramsB, resolved := resolveParamCountB(modelName)
	if !resolved {
		return map[string]any{
			"model_name": modelName,
			"resolved":   false,
			"note":       "无法从模型名识别参数量。请提供参数量(如 32B / 7B / 70B)或确认模型全称(如 Qwen2.5-32B-Instruct),我再估算显存与可承载的 GPU。",
		}
	}

	vramRequired := int(math.Ceil(paramsB * bpp * vramBufferFactor))

	singleCard, multiCard := fitGPUs(vramRequired)

	result := map[string]any{
		"model_name":       modelName,
		"resolved":         true,
		"params_b":         paramsB,
		"quantization":     quant,
		"bytes_per_param":  bpp,
		"buffer_factor":    vramBufferFactor,
		"vram_required_gb": vramRequired,
		"note":             "显存估算 = 参数量 × 每参数字节 × 1.2 buffer(覆盖 KV-cache/激活/框架开销);长上下文或高并发需更多显存,int8/int4 量化可显著降低。价格用 GetCompShareInstancePrice、库存用 CheckCompShareResourceCapacity 确认;算力/场景优选可叠加 GetGPURecommendation。",
	}
	if len(singleCard) > 0 {
		result["single_card_options"] = singleCard
		result["recommended"] = singleCard[0] // smallest VRAM that fits = least waste
	} else if multiCard != nil {
		result["single_card_options"] = []gpuFit{}
		result["multi_card_fallback"] = *multiCard
		result["recommended"] = *multiCard
	}
	return result
}

// RecommendGPUType picks a single CreateInstance GpuType string for a deploy
// request, plus a short human note explaining the choice. When modelName
// resolves to a known parameter count it sizes the GPU by VRAM (same arithmetic
// as GetModelVRAMRequirement, smallest single card that fits, else the
// largest-card multi-card fallback); otherwise it falls back to the
// scene-keyword recommendation (GetGPURecommendation). It always returns a
// non-empty GpuType. Pure / deterministic — the deploy_model arm calls this
// instead of the map[string]any tool surface so it gets a typed value rather
// than an unexported gpuFit hidden behind `any` (B8.3).
func RecommendGPUType(modelName, quantization, scene string) (gpuType, note string) {
	if paramsB, ok := resolveParamCountB(modelName); ok {
		quant := strings.ToLower(strings.TrimSpace(quantization))
		bpp, has := bytesPerParam[quant]
		if !has {
			quant = "fp16"
			bpp = bytesPerParam[quant]
		}
		vramRequired := int(math.Ceil(paramsB * bpp * vramBufferFactor))
		single, multi := fitGPUs(vramRequired)
		if len(single) > 0 {
			return single[0].GPUType, fmt.Sprintf("%s(约 %gB,%s)需约 %dGB 显存,选单卡 %s(%dGB)", modelName, paramsB, quant, vramRequired, single[0].Name, single[0].VRAMGB)
		}
		if multi != nil {
			return multi.GPUType, fmt.Sprintf("%s(约 %gB,%s)需约 %dGB 显存,%s", modelName, paramsB, quant, vramRequired, multi.Note)
		}
	}
	// No recognized parameter count — fall back to scene-keyword recommendation.
	rec := GetGPURecommendation(scene, false)
	if recs, ok := rec["recommendations"].([]GPURec); ok && len(recs) > 0 {
		return recs[0].GPUType, fmt.Sprintf("按场景推荐 %s(%dGB)", recs[0].Name, recs[0].VRAM)
	}
	// Last-resort default — a broadly available mid-tier card. GetGPURecommendation
	// always returns ≥1 rec today, so this only guards a future regression.
	return "4090", "未能识别模型参数量或场景,默认推荐 4090(24GB)"
}

// RecommendGPUTypeWithin is RecommendGPUType constrained to an image's declared
// SupportedGpuTypes (B8.3 M2). The upstream DescribeCompShareImages list response
// carries SupportedGpuTypes as the platform's recommended cards for that image,
// in the SAME bare form as the gpuSpecs keys ("4090"/"V100S"/"A100"; verified by
// live recon 2026-05-31). Policy:
//   - allowed empty (image declares none, e.g. Ollama/SGLang) → identical to
//     RecommendGPUType: no constraint to apply.
//   - unconstrained pick already supported → keep it (the common case; most images
//     support all cards).
//   - otherwise re-pick within the supported set: for a sized model, the smallest
//     supported card whose VRAM ≥ requirement (still fits). When NO supported card
//     meets the requirement, KEEP the unconstrained (correctly sized) pick and only
//     warn — SupportedGpuTypes is a recommendation, not a hard cap, and creating a
//     too-small instance is worse than deviating from the suggested list.
//   - scene-based (no model size): prefer a scene-recommended supported card, else
//     the most capable supported card.
//
// It always returns a non-empty GpuType. Pure / deterministic.
func RecommendGPUTypeWithin(modelName, quantization, scene string, allowed []string) (gpuType, note string) {
	base, baseNote := RecommendGPUType(modelName, quantization, scene)
	allowedSet := normalizeAllowedGPUs(allowed)
	if len(allowedSet) == 0 || allowedSet[base] {
		return base, baseNote
	}

	// Unconstrained pick is not in the image's supported list — re-pick within it.
	if paramsB, ok := resolveParamCountB(modelName); ok {
		quant := strings.ToLower(strings.TrimSpace(quantization))
		bpp, has := bytesPerParam[quant]
		if !has {
			bpp = bytesPerParam["fp16"]
		}
		required := int(math.Ceil(paramsB * bpp * vramBufferFactor))
		if key, spec, found := smallestAllowedFitting(allowedSet, required); found {
			return key, fmt.Sprintf("%s 约需 %dGB 显存；该镜像支持机型 %v，据此选支持的 %s(%dGB)（原推荐 %s）",
				modelName, required, sortedAllowedGPUs(allowedSet), spec.Name, spec.VRAM, base)
		}
		// No supported card fits — keep the correctly-sized pick, just inform.
		return base, fmt.Sprintf("%s；注意该镜像声明支持的机型 %v 可能不满足该模型显存需求，已按显存需求保留 %s",
			baseNote, sortedAllowedGPUs(allowedSet), base)
	}

	// Scene-based (no model size): prefer a scene-recommended supported card.
	rec := GetGPURecommendation(scene, false)
	if recs, ok := rec["recommendations"].([]GPURec); ok {
		for _, r := range recs {
			if allowedSet[r.GPUType] {
				return r.GPUType, fmt.Sprintf("按场景在镜像支持机型 %v 中选 %s(%dGB)（原推荐 %s）",
					sortedAllowedGPUs(allowedSet), gpuSpecs[r.GPUType].Name, gpuSpecs[r.GPUType].VRAM, base)
			}
		}
	}
	// No scene overlap — pick the most capable supported card (highest FP16).
	if key, spec, found := mostCapableAllowed(allowedSet); found {
		return key, fmt.Sprintf("镜像支持机型 %v 中选算力最高的 %s(%dGB)（原推荐 %s）",
			sortedAllowedGPUs(allowedSet), spec.Name, spec.VRAM, base)
	}
	return base, baseNote
}

// normalizeAllowedGPUs dedups the allowed list and keeps only entries that are
// known gpuSpecs keys (the live SupportedGpuTypes can contain duplicates and, in
// principle, keys we don't model — both are dropped so callers see a clean set).
func normalizeAllowedGPUs(allowed []string) map[string]bool {
	set := make(map[string]bool, len(allowed))
	for _, g := range allowed {
		g = strings.TrimSpace(g)
		if _, known := gpuSpecs[g]; known {
			set[g] = true
		}
	}
	return set
}

func sortedAllowedGPUs(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// smallestAllowedFitting returns the supported card with the least VRAM that still
// meets vramRequired (ties broken by higher FP16, then lexicographically smaller
// key for determinism).
func smallestAllowedFitting(set map[string]bool, vramRequired int) (key string, spec GPUSpec, found bool) {
	keys := sortedAllowedGPUs(set)
	for _, k := range keys {
		s := gpuSpecs[k]
		if s.VRAM < vramRequired {
			continue
		}
		if !found || s.VRAM < spec.VRAM || (s.VRAM == spec.VRAM && s.FP16 > spec.FP16) {
			key, spec, found = k, s, true
		}
	}
	return key, spec, found
}

// mostCapableAllowed returns the supported card with the highest FP16 (ties broken
// by higher VRAM, then lexicographically smaller key).
func mostCapableAllowed(set map[string]bool) (key string, spec GPUSpec, found bool) {
	keys := sortedAllowedGPUs(set)
	for _, k := range keys {
		s := gpuSpecs[k]
		if !found || s.FP16 > spec.FP16 || (s.FP16 == spec.FP16 && s.VRAM > spec.VRAM) {
			key, spec, found = k, s, true
		}
	}
	return key, spec, found
}

// resolveParamCountB derives a model's parameter count (billions) from its name,
// preferring the canonical table for count-less / MoE names, then falling back
// to the last "<n>b" token in the name (last wins so "qwen3-32b" → 32, not 3).
func resolveParamCountB(modelName string) (float64, bool) {
	norm := normalizeModelName(modelName)
	if p, ok := canonicalModels[norm]; ok {
		return p, true
	}
	matches := paramCountRE.FindAllStringSubmatch(modelName, -1)
	if len(matches) == 0 {
		return 0, false
	}
	last := matches[len(matches)-1][1]
	var p float64
	if _, err := fmt.Sscanf(last, "%g", &p); err != nil || p <= 0 {
		return 0, false
	}
	return p, true
}

// normalizeModelName lowercases and strips spaces/underscores/hyphens/dots so
// "DeepSeek-V3" / "deepseek v3" / "deepseek_v3" all hit the canonical key.
func normalizeModelName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	r := strings.NewReplacer(" ", "", "_", "", "-", "", ".", "")
	return r.Replace(s)
}

// gpuFit is one GPU configuration that holds the model.
type gpuFit struct {
	GPUType     string `json:"gpu_type"`
	Name        string `json:"name"`
	VRAMGB      int    `json:"vram_gb"`
	Cards       int    `json:"cards"`
	TotalVRAMGB int    `json:"total_vram_gb"`
	Note        string `json:"note"`
}

// fitGPUs returns single-card options (one card per VRAM tier whose card fits,
// ascending by VRAM, best FP16 per tier) and — when no single card fits — a
// multi-card fallback using the largest available GPU.
func fitGPUs(vramRequired int) (single []gpuFit, multi *gpuFit) {
	// Best card (highest FP16) per distinct VRAM tier, so we don't list a weak
	// P40 alongside a 4090 at the same 24GB. Iterate keys in SORTED order so
	// ties (e.g. 4090 vs 4090Pro, both 24GB/82.6 TFLOPS) resolve deterministically
	// to the lexicographically smaller key (map iteration order is random in Go).
	keys := make([]string, 0, len(gpuSpecs))
	for key := range gpuSpecs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	bestPerVRAM := map[int]GPUSpec{}
	bestKey := map[int]string{}
	for _, key := range keys {
		spec := gpuSpecs[key]
		if cur, ok := bestPerVRAM[spec.VRAM]; !ok || spec.FP16 > cur.FP16 {
			bestPerVRAM[spec.VRAM] = spec
			bestKey[spec.VRAM] = key
		}
	}
	vrams := make([]int, 0, len(bestPerVRAM))
	for v := range bestPerVRAM {
		vrams = append(vrams, v)
	}
	sort.Ints(vrams)

	for _, v := range vrams {
		if v >= vramRequired {
			spec := bestPerVRAM[v]
			single = append(single, gpuFit{
				GPUType: bestKey[v], Name: spec.Name, VRAMGB: v, Cards: 1, TotalVRAMGB: v,
				Note: fmt.Sprintf("单卡 %dGB ≥ 需求 %dGB", v, vramRequired),
			})
		}
	}
	if len(single) > 0 {
		return single, nil
	}

	// No single card fits — fall back to N× the largest GPU.
	maxVRAM := vrams[len(vrams)-1]
	spec := bestPerVRAM[maxVRAM]
	cards := int(math.Ceil(float64(vramRequired) / float64(maxVRAM)))
	if cards > spec.MaxGPU {
		// Beyond a single host's card limit — still report it; the skill warns.
		return nil, &gpuFit{
			GPUType: bestKey[maxVRAM], Name: spec.Name, VRAMGB: maxVRAM, Cards: cards,
			TotalVRAMGB: cards * maxVRAM,
			Note:        fmt.Sprintf("需 %d×%s(%dGB)=%dGB,超过单机最大 %d 卡;需多机或更激进量化", cards, spec.Name, maxVRAM, cards*maxVRAM, spec.MaxGPU),
		}
	}
	return nil, &gpuFit{
		GPUType: bestKey[maxVRAM], Name: spec.Name, VRAMGB: maxVRAM, Cards: cards,
		TotalVRAMGB: cards * maxVRAM,
		Note:        fmt.Sprintf("无单卡可承载;%d×%s(%dGB)=%dGB ≥ 需求 %dGB", cards, spec.Name, maxVRAM, cards*maxVRAM, vramRequired),
	}
}
