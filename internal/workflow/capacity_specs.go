package workflow

// capacity_specs.go — a single, reusable reading of the CheckCompShareResourceCapacity
// response. Availability AND the valid GPU-count / CPU-memory combinations for a
// (Zone, GpuType, Image, Disk) draft all come from that one call's Specs[]:
// each element is {Gpu, Cpu, Mem, ResourceEnough} (see the production API doc
// docs/api/spec/CheckCompShareResourceCapacity.md and stepCheckCapacity). The
// official compshare-cli builds inventory the same way — in_stock = ResourceEnough
// per spec — and deliberately does NOT trust DescribeCompShareGpuInventory raw
// counts (which false-negative some zones, e.g. cn-bj2-03). These helpers are pure
// and side-effect free so the guided GPU-count / CPU-memory option builders can be
// gated by real creatability instead of the static legal catalog + raw-count notes.

// capacitySpec is one {Gpu,Cpu,Mem} combination and whether upstream can create it now.
type capacitySpec struct {
	GPU    int // GPU card count
	CPU    int // CPU cores
	MemGB  int // memory in GB (Specs.Mem is already GB, unlike the draft's MB)
	Enough bool
}

// parseCapacitySpecs reads the Specs[] array of a CheckCompShareResourceCapacity
// result. Returns nil for a missing/empty/foreign-shaped payload — callers treat
// "no capacity signal" as "fall back to the legal catalog", never as "no stock".
func parseCapacitySpecs(result map[string]any) []capacitySpec {
	if result == nil {
		return nil
	}
	raw, ok := result["Specs"].([]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	out := make([]capacitySpec, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		gpu, gok := capacityInt(m["Gpu"])
		cpu, cok := capacityInt(m["Cpu"])
		mem, mok := capacityInt(m["Mem"])
		if !gok || !cok || !mok {
			continue
		}
		out = append(out, capacitySpec{GPU: gpu, CPU: cpu, MemGB: mem, Enough: capacityBool(m["ResourceEnough"])})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// capacityHasSignal reports whether a usable Specs[] was returned at all. When
// false, the option builders must keep their legal-catalog behavior — absence of
// a capacity signal is NOT evidence of unavailability.
func capacityHasSignal(specs []capacitySpec) bool { return len(specs) > 0 }

// capacityCreatable reports zone-level creatability: at least one spec is enough.
// This is the availability truth to prefer over a raw GPU-inventory count.
func capacityCreatable(specs []capacitySpec) bool {
	for _, s := range specs {
		if s.Enough {
			return true
		}
	}
	return false
}

// capacityGPUCountEnough reports whether the given GPU count has any ResourceEnough
// spec — used to enable/disable a 卡数量 option.
func capacityGPUCountEnough(specs []capacitySpec, gpuCount int) bool {
	for _, s := range specs {
		if s.GPU == gpuCount && s.Enough {
			return true
		}
	}
	return false
}

// capacityKnowsGPUCount reports whether the capacity result enumerated the given
// GPU count at all. A count the probe never evaluated must not be hard-disabled on
// capacity — only a KNOWN-but-short count is.
func capacityKnowsGPUCount(specs []capacitySpec, gpuCount int) bool {
	for _, s := range specs {
		if s.GPU == gpuCount {
			return true
		}
	}
	return false
}

// capacityCPUMemEnough reports whether a specific (GPU count, CPU, Mem-GB) combo is
// creatable — used to enable/disable a CPU/内存 option.
func capacityCPUMemEnough(specs []capacitySpec, gpuCount, cpu, memGB int) bool {
	for _, s := range specs {
		if s.GPU == gpuCount && s.CPU == cpu && s.MemGB == memGB {
			return s.Enough
		}
	}
	return false
}

// capacityKnowsCombo reports whether the capacity result even mentions this
// (GPU,CPU,Mem) combo. A combo the capacity call never enumerated must not be
// hard-disabled purely on capacity — it may be a legal spec the probe image/disk
// simply did not exercise; the final 检查库存 step re-checks the sealed config.
func capacityKnowsCombo(specs []capacitySpec, gpuCount, cpu, memGB int) bool {
	for _, s := range specs {
		if s.GPU == gpuCount && s.CPU == cpu && s.MemGB == memGB {
			return true
		}
	}
	return false
}

func capacityInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case float32:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	default:
		return 0, false
	}
}

func capacityBool(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return b == "true" || b == "True"
	default:
		return false
	}
}
