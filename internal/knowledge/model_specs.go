package knowledge

import (
	"fmt"
	"regexp"
	"strings"
)

// model_specs.go estimates model-side VRAM demand from a model name. It contains
// no platform GPU catalog; FittingGPUAlternatives combines the estimate with
// live catalog entries.

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
// and framework overhead. The deploy_model rule is to keep the estimate
// conservative with a 20-30% buffer; 1.2 is the single-request floor, and long
// context or high concurrency needs more.
const vramBufferFactor = 1.2

// paramCountRE matches a parameter count embedded in a model name: an integer
// or decimal immediately followed by 'b'/'B' (billions), e.g. "32b", "70B",
// "1.5b", "Qwen32B", "Llama3-70B", "deepseek-67b". The leading boundary keeps
// "3b" in "fp3b8" from matching while allowing "qwen3-32b" → 32 (the LAST such
// token wins, see resolveParamCountB).
var paramCountRE = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*b\b`)

// canonicalModels maps count-less model names whose size is UNAMBIGUOUS to their
// parameter total. Keys are normalized (lowercased, spaces/underscores stripped).
// Values are TOTAL parameter count in billions — for MoE models the total (all
// experts) is correct because every expert must be resident in VRAM even though
// only some are active per token.
//
// Multi-size families (DeepSeek-R1/V3/V2, Qwen, Llama, …) are deliberately
// ABSENT: a bare "DeepSeek R1" spans 1.5B–671B, so defaulting to one total
// silently sizes for a config the user never asked for. Those names resolve to
// unresolved on purpose. Only genuinely single-variant or regex-trap names
// belong here.
//
// This is a MODEL fact, not a platform fact — it stays local because it does not
// describe CompShare's fleet and does not drift when the platform changes cards.
var canonicalModels = map[string]float64{
	"mixtral8x7b":  47,  // MoE total (~46.7B); regex would wrongly read "7b" → 7
	"mixtral8x22b": 141, // MoE total (~141B)
	"qwq":          32,  // QwQ-32B (single variant; useful when size omitted)
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
