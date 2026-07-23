package workflow

import "testing"

// sample mirrors the production API doc's response example
// (docs/api/spec/CheckCompShareResourceCapacity.md): two 1-GPU specs enough, a
// 2-GPU spec not enough. Values arrive as float64 (JSON) like the live executor.
func sampleCapacityResult() map[string]any {
	return map[string]any{
		"Specs": []any{
			map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(32), "ResourceEnough": true},
			map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true},
			map[string]any{"Gpu": float64(2), "Cpu": float64(32), "Mem": float64(128), "ResourceEnough": false},
		},
	}
}

func TestParseCapacitySpecs(t *testing.T) {
	specs := parseCapacitySpecs(sampleCapacityResult())
	if len(specs) != 3 {
		t.Fatalf("want 3 specs, got %d", len(specs))
	}
	if specs[0] != (capacitySpec{GPU: 1, CPU: 16, MemGB: 32, Enough: true}) {
		t.Fatalf("spec[0] mismatch: %+v", specs[0])
	}
	if specs[2] != (capacitySpec{GPU: 2, CPU: 32, MemGB: 128, Enough: false}) {
		t.Fatalf("spec[2] mismatch: %+v", specs[2])
	}
}

func TestParseCapacitySpecsAbsentIsNilNotEmpty(t *testing.T) {
	// A missing / empty / foreign payload yields nil so callers fall back to the
	// legal catalog — "no signal" must never read as "no stock".
	for name, in := range map[string]map[string]any{
		"nil":         nil,
		"no Specs":    {"RetCode": float64(0)},
		"empty Specs": {"Specs": []any{}},
		"bad shape":   {"Specs": []any{"nope", map[string]any{"Cpu": float64(8)}}},
	} {
		if got := parseCapacitySpecs(in); got != nil {
			t.Errorf("%s: want nil, got %+v", name, got)
		}
		if capacityHasSignal(parseCapacitySpecs(in)) {
			t.Errorf("%s: capacityHasSignal should be false", name)
		}
	}
}

func TestCapacityCreatable(t *testing.T) {
	specs := parseCapacitySpecs(sampleCapacityResult())
	if !capacityCreatable(specs) {
		t.Fatal("want creatable: two 1-GPU specs are enough")
	}
	// The cn-bj2-03 (华北一C) shape we confirmed live: raw inventory is empty but
	// the capacity call reports enough — creatable must be true.
	allShort := []capacitySpec{{GPU: 2, CPU: 32, MemGB: 128, Enough: false}}
	if capacityCreatable(allShort) {
		t.Fatal("want not creatable when every spec is short")
	}
}

func TestCapacityGPUCountAndCPUMemGating(t *testing.T) {
	specs := parseCapacitySpecs(sampleCapacityResult())
	if !capacityGPUCountEnough(specs, 1) {
		t.Error("1-GPU should be enough")
	}
	if capacityGPUCountEnough(specs, 2) {
		t.Error("2-GPU should NOT be enough (its only spec is short)")
	}
	if !capacityKnowsGPUCount(specs, 2) {
		t.Error("2-GPU is enumerated (short) → known, so it may be hard-disabled")
	}
	if capacityKnowsGPUCount(specs, 4) {
		t.Error("4-GPU is not enumerated → not known, must stay enabled")
	}
	if !capacityCPUMemEnough(specs, 1, 16, 64) {
		t.Error("1x16C/64G should be enough")
	}
	if capacityCPUMemEnough(specs, 2, 32, 128) {
		t.Error("2x32C/128G should NOT be enough")
	}
	// A combo the capacity call never enumerated: not enough, but also not "known"
	// — the builder must not hard-disable it on capacity alone.
	if capacityCPUMemEnough(specs, 1, 8, 16) {
		t.Error("unenumerated combo should not report enough")
	}
	if capacityKnowsCombo(specs, 1, 8, 16) {
		t.Error("unenumerated combo should not be 'known'")
	}
	if !capacityKnowsCombo(specs, 2, 32, 128) {
		t.Error("enumerated (even if short) combo should be 'known'")
	}
}
