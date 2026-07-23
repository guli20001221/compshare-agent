package knowledge

import (
	"math"
	"sort"
	"strings"
)

// gpu_live.go reads the platform's GPU facts from the upstream catalog.
//
// It once hedged: a live path plus a hand-maintained gpuSpecs table as an
// "offline fallback" for when the query failed. Both are gone. The table was
// deleted (it drifts every time the platform adds, retires or renames a card),
// and with it RecommendGPUTypeLive — which turned out to have had ZERO production
// callers, so the "live GPU sizing" it advertised never actually ran. What
// remains is the part that IS wired: parsing the catalog, and picking fallback
// cards from it when a create hits a sold-out tier.
//
// There is deliberately no fallback left. A caller that cannot reach the catalog
// must say it cannot confirm, not answer from a local copy.
//
// Upstream contract (pkg/api/describe_available_compshare_instance_types.go):
// AvailableInstanceTypes[] entries carry Name (== CreateInstance GpuType), Status
// (availability), GraphicsMemory.Value (VRAM GB), Performance.Value (a perf score
// used for the per-tier tiebreak in place of the static FP16), and
// MachineSizes[].Gpu (the max card count). All survive the JSON round-trip as
// map[string]any with the nested {Rate,Value} shape the agent already parses
// elsewhere (route_registry.go).

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

// FittingGPUAlternatives returns up to `limit` of the `available` cards a deploy
// can fall back to when its recommended card (`exclude`) is sold out, image-compat
// filtered by `allowed` (the image's SupportedGpuTypes; dropped when it doesn't
// intersect what's offered).
//
// Two ranking regimes:
//   - Known model size (LLM): keep only VRAM-sufficient cards, ranked cheapest-
//     sufficient first (smallest fitting VRAM; ties → higher perf, then name) to
//     minimise over-provisioning.
//   - Unknown model size (an app/image deploy — ComfyUI / SD-WebUI / 数字人 — that
//     has no parameter count): no VRAM floor (any image-compatible card runs the
//     app), ranked strongest-first (highest perf, then larger VRAM), the same
//     preference the scene path uses (mostPerfAvailable).
//
// Returns nil only when nothing is offered after filtering, so callers degrade to
// a generic message.
func FittingGPUAlternatives(modelName, quantization string, allowed []string, available []AvailableGPU, exclude string, limit int) []AvailableGPU {
	required, sized := 0, false
	if paramsB, ok := resolveParamCountB(modelName); ok {
		required, sized = liveVRAMRequired(paramsB, quantization), true
	}
	pool := available
	if allowedPool := filterAvailableByNames(available, allowed); len(allowedPool) > 0 {
		pool = allowedPool
	}
	var out []AvailableGPU
	for _, g := range pool {
		if strings.EqualFold(strings.TrimSpace(g.Name), strings.TrimSpace(exclude)) {
			continue
		}
		if sized && g.VRAMGB < required {
			continue
		}
		out = append(out, g)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if sized {
			// LLM: cheapest sufficient first (smallest fitting VRAM; ties → higher perf).
			if out[i].VRAMGB != out[j].VRAMGB {
				return out[i].VRAMGB < out[j].VRAMGB
			}
			if out[i].Perf != out[j].Perf {
				return out[i].Perf > out[j].Perf
			}
		} else {
			// App/image: strongest first (highest perf; ties → larger VRAM).
			if out[i].Perf != out[j].Perf {
				return out[i].Perf > out[j].Perf
			}
			if out[i].VRAMGB != out[j].VRAMGB {
				return out[i].VRAMGB > out[j].VRAMGB
			}
		}
		return out[i].Name < out[j].Name
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
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


// WithoutGPUTypes drops the named cards from a recommendation list.
//
// It exists because the availability catalog cannot express charge-type
// eligibility. DescribeAvailableCompShareInstanceTypes answers InstanceType=spot
// with an empty list rather than a Spot-scoped one, so "what can I offer instead
// on Spot" has to be built as: take the full catalog, then subtract the cards
// that upstream does not sell on Spot at all
// (DescribeCompShareGpuInventory.SpotUnsupportedGpuTypes).
//
// Empty exclusions return the input unchanged — a missing inventory answer must
// not silently empty the recommendation.
func WithoutGPUTypes(available []AvailableGPU, exclude []string) []AvailableGPU {
	if len(exclude) == 0 {
		return available
	}
	set := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		if n := strings.ToLower(strings.TrimSpace(e)); n != "" {
			set[n] = true
		}
	}
	var out []AvailableGPU
	for _, g := range available {
		if !set[strings.ToLower(strings.TrimSpace(g.Name))] {
			out = append(out, g)
		}
	}
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
