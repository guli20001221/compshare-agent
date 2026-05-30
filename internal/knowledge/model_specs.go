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
