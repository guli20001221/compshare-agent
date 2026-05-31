package knowledge

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// gpu_live.go makes deploy GPU sizing API-driven (B8.3 follow-up, 2026-05-31).
//
// The hand-maintained gpuSpecs table (model_specs.go / gpu_specs.go) goes STALE
// when the platform adds a new card, retires one, or sells out a tier — exactly
// the lead's concern. Instead of relying on it, the deploy matcher now sizes the
// model against the LIVE set returned by DescribeAvailableCompShareInstanceTypes,
// which by construction reflects only currently-offered cards. The static table
// remains the OFFLINE FALLBACK (RecommendGPUTypeWithin) for when the live query is
// empty or unavailable, so a transient API failure degrades gracefully rather than
// blocking a deploy.
//
// Upstream contract (pkg/api/describe_available_compshare_instance_types.go):
// AvailableInstanceTypes[] entries carry Name (== CreateInstance GpuType), Status
// (availability), GraphicsMemory.Value (VRAM GB), Performance.Value (a perf score
// used for the per-tier tiebreak in place of the static FP16), and
// MachineSizes[].Gpu (the max card count). All survive the JSON round-trip as
// map[string]any with the nested {Rate,Value} shape the agent already parses
// elsewhere (capability_registry.go).

// AvailableGPU is one live, currently-offered GPU model.
type AvailableGPU struct {
	Name   string  // matches CreateInstance GpuType, e.g. "4090"
	VRAMGB int     // GraphicsMemory.Value
	Perf   float64 // Performance.Value — higher = faster; tiebreak within a VRAM tier
	MaxGPU int     // max single-host card count across MachineSizes
}

// ParseAvailableGPUs extracts the available GPU candidates from a
// DescribeAvailableCompShareInstanceTypes result map, restricted to the given zone.
//
// The response spans MULTIPLE zones (verified 2026-05-31: region cn-wlcb returns
// both cn-wlcb-01 AND the Shanghai cn-sh2-02 zone — the upstream returns every
// zone's machine types when no ZoneID is filtered). The deploy saga creates in ONE
// zone, so we keep only that zone's cards; otherwise a card offered solely in
// another zone (e.g. 2080Ti only in cn-sh2-02) would be recommended and then
// rejected by the saga's zone-scoped capacity check. zone=="" disables the filter
// (keep all zones). Entries without a Name or positive VRAM, or whose Status marks
// them unavailable, are skipped; duplicate Names collapse (keeping the larger
// MaxGPU). Returns nil for a nil/empty result so callers fall back to the static table.
func ParseAvailableGPUs(result map[string]any, zone string) []AvailableGPU {
	if result == nil {
		return nil
	}
	zone = strings.TrimSpace(zone)
	types, _ := result["AvailableInstanceTypes"].([]any)
	byName := map[string]AvailableGPU{}
	var order []string
	for _, t := range types {
		m, _ := t.(map[string]any)
		if m == nil {
			continue
		}
		if zone != "" && !strings.EqualFold(strings.TrimSpace(stringField(m, "Zone")), zone) {
			continue
		}
		name := strings.TrimSpace(stringField(m, "Name"))
		if name == "" || !availableStatus(stringField(m, "Status")) {
			continue
		}
		vram := intFromNested(m["GraphicsMemory"], "Value")
		if vram <= 0 {
			continue
		}
		g := AvailableGPU{
			Name:   name,
			VRAMGB: vram,
			Perf:   floatFromNested(m["Performance"], "Value"),
			MaxGPU: maxGpuFromMachineSizes(m["MachineSizes"]),
		}
		if prev, ok := byName[name]; ok {
			if g.MaxGPU > prev.MaxGPU {
				prev.MaxGPU = g.MaxGPU
				byName[name] = prev
			}
			continue
		}
		byName[name] = g
		order = append(order, name)
	}
	out := make([]AvailableGPU, 0, len(order))
	for _, n := range order {
		out = append(out, byName[n])
	}
	return out
}

// RecommendGPUTypeLive sizes a deploy request against the LIVE available-card set,
// applying the same image-constraint (M2) policy as RecommendGPUTypeWithin but over
// currently-offered cards instead of the static table. allowed is the chosen
// image's SupportedGpuTypes (may be empty = no constraint). When available is empty
// (offline / query failed) it falls back to the static-table RecommendGPUTypeWithin
// so behavior degrades gracefully. Always returns a non-empty GpuType.
func RecommendGPUTypeLive(modelName, quantization, scene string, allowed []string, available []AvailableGPU) (gpuType, note string) {
	if len(available) == 0 {
		return RecommendGPUTypeWithin(modelName, quantization, scene, allowed)
	}
	// Image-supported AND currently-available cards (M2 ∩ live).
	allowedPool := filterAvailableByNames(available, allowed)
	hasConstraint := len(allowed) > 0 && len(allowedPool) > 0

	if paramsB, ok := resolveParamCountB(modelName); ok {
		required := liveVRAMRequired(paramsB, quantization)
		// Prefer an image-supported, available, fitting card.
		if hasConstraint {
			if g, ok := smallestFittingAvailable(allowedPool, required); ok {
				return g.Name, fmt.Sprintf("%s 约需 %dGB 显存；镜像支持且当前可用机型中选 %s(%dGB)", modelName, required, g.Name, g.VRAMGB)
			}
		}
		// Else any available fitting card (VRAM-correctness wins over the image's
		// advisory list — same policy as the static path).
		if g, ok := smallestFittingAvailable(available, required); ok {
			if hasConstraint {
				return g.Name, fmt.Sprintf("%s 约需 %dGB 显存；镜像推荐机型不满足显存，按当前可用机型选 %s(%dGB)", modelName, required, g.Name, g.VRAMGB)
			}
			return g.Name, fmt.Sprintf("%s 约需 %dGB 显存，按当前可用机型选 %s(%dGB)", modelName, required, g.Name, g.VRAMGB)
		}
		// No single available card fits → multi-card on the largest available.
		largest := largestVRAMAvailable(available)
		cards := int(math.Ceil(float64(required) / float64(largest.VRAMGB)))
		if largest.MaxGPU > 0 && cards > largest.MaxGPU {
			return largest.Name, fmt.Sprintf("%s 约需 %dGB 显存；%d×%s(%dGB) 超单机 %d 卡上限，建议更激进量化或多机", modelName, required, cards, largest.Name, largest.VRAMGB, largest.MaxGPU)
		}
		return largest.Name, fmt.Sprintf("%s 约需 %dGB 显存；无单卡可承载，按当前可用机型用 %d×%s(%dGB)", modelName, required, cards, largest.Name, largest.VRAMGB)
	}

	// Scene-based (no recognizable model size).
	pool, suffix := available, ""
	if hasConstraint {
		pool, suffix = allowedPool, "（镜像支持机型内）"
	}
	if g, ok := sceneCardAvailable(scene, pool); ok {
		return g.Name, fmt.Sprintf("按场景在当前可用机型%s中选 %s(%dGB)", suffix, g.Name, g.VRAMGB)
	}
	best := mostPerfAvailable(pool)
	if best.Name == "" {
		best = mostPerfAvailable(available) // allowed∩available empty → widen to all available
		suffix = ""
	}
	return best.Name, fmt.Sprintf("按当前可用机型%s选算力较高的 %s(%dGB)", suffix, best.Name, best.VRAMGB)
}

// liveVRAMRequired mirrors the static VRAM arithmetic (params × bytes × buffer).
func liveVRAMRequired(paramsB float64, quantization string) int {
	bpp, ok := bytesPerParam[strings.ToLower(strings.TrimSpace(quantization))]
	if !ok {
		bpp = bytesPerParam["fp16"]
	}
	return int(math.Ceil(paramsB * bpp * vramBufferFactor))
}

// filterAvailableByNames keeps only available cards whose Name is in allowed
// (case-insensitive). allowed empty → returns nil (caller treats as no constraint).
func filterAvailableByNames(available []AvailableGPU, allowed []string) []AvailableGPU {
	if len(allowed) == 0 {
		return nil
	}
	set := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		set[strings.ToLower(strings.TrimSpace(a))] = true
	}
	var out []AvailableGPU
	for _, g := range available {
		if set[strings.ToLower(g.Name)] {
			out = append(out, g)
		}
	}
	return out
}

// smallestFittingAvailable returns the available card with the least VRAM that
// still meets vramRequired (ties → higher Perf, then lexicographic Name for
// determinism since the API slice order is not guaranteed).
func smallestFittingAvailable(cards []AvailableGPU, vramRequired int) (AvailableGPU, bool) {
	var best AvailableGPU
	found := false
	for _, g := range sortedByName(cards) {
		if g.VRAMGB < vramRequired {
			continue
		}
		if !found || g.VRAMGB < best.VRAMGB || (g.VRAMGB == best.VRAMGB && g.Perf > best.Perf) {
			best, found = g, true
		}
	}
	return best, found
}

// largestVRAMAvailable returns the card with the most VRAM (ties → higher Perf).
func largestVRAMAvailable(cards []AvailableGPU) AvailableGPU {
	var best AvailableGPU
	for _, g := range sortedByName(cards) {
		if best.Name == "" || g.VRAMGB > best.VRAMGB || (g.VRAMGB == best.VRAMGB && g.Perf > best.Perf) {
			best = g
		}
	}
	return best
}

// mostPerfAvailable returns the card with the highest Perf (ties → larger VRAM).
func mostPerfAvailable(cards []AvailableGPU) AvailableGPU {
	var best AvailableGPU
	for _, g := range sortedByName(cards) {
		if best.Name == "" || g.Perf > best.Perf || (g.Perf == best.Perf && g.VRAMGB > best.VRAMGB) {
			best = g
		}
	}
	return best
}

// sceneCardAvailable maps the scene to GetGPURecommendation's order and returns the
// first recommended card that is in the available pool.
func sceneCardAvailable(scene string, pool []AvailableGPU) (AvailableGPU, bool) {
	rec := GetGPURecommendation(scene, false)
	recs, _ := rec["recommendations"].([]GPURec)
	for _, r := range recs {
		for _, g := range pool {
			if strings.EqualFold(g.Name, r.GPUType) {
				return g, true
			}
		}
	}
	return AvailableGPU{}, false
}

func sortedByName(cards []AvailableGPU) []AvailableGPU {
	out := append([]AvailableGPU(nil), cards...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// availableStatus reports whether a machine-type Status means the card is sellable.
// The upstream enum is exactly Normal(可售)/SoldOut(售罄)
// (DescribeAvailableCompShareInstanceTypes spec). We ALLOW-LIST: only "normal" (or
// an empty status, for responses that omit the field) counts as available; any
// other value — including a future non-sellable status the platform might add —
// fails CLOSED, so a card we can't confirm is sellable is never recommended for a
// deploy. Matches the rest of the codebase, which treats Status as binary.
func availableStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "normal":
		return true
	default:
		return false
	}
}

func stringField(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

// intFromNested reads m[key].Value from a nested {Rate,Value} map, coercing the
// JSON-decoded number (float64) to int.
func intFromNested(v any, key string) int {
	m, ok := v.(map[string]any)
	if !ok {
		return 0
	}
	switch n := m[key].(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}

func floatFromNested(v any, key string) float64 {
	m, ok := v.(map[string]any)
	if !ok {
		return 0
	}
	switch n := m[key].(type) {
	case float64:
		return n
	case int:
		return float64(n)
	default:
		return 0
	}
}

// maxGpuFromMachineSizes returns the largest Gpu count across MachineSizes[].
func maxGpuFromMachineSizes(v any) int {
	sizes, ok := v.([]any)
	if !ok {
		return 0
	}
	max := 0
	for _, s := range sizes {
		m, _ := s.(map[string]any)
		if m == nil {
			continue
		}
		switch g := m["Gpu"].(type) {
		case float64:
			if int(g) > max {
				max = int(g)
			}
		case int:
			if g > max {
				max = g
			}
		}
	}
	return max
}
