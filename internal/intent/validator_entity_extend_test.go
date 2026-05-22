package intent

// C15 Phase A — strict zero-behavior tests.
//
// Phase A introduces TargetRefZone / TargetRefImage / TargetRefGPUModel as
// type constants in types.go but DELIBERATELY does not change the
// validator. The new types remain rejected by validateTargetRef just
// like any other unknown type (default case in the switch) — see
// validator.go comment block. This file pins that contract so a
// future edit to the validator switch is review-visible.
//
// Phase B will add the validator switch case + planner directives +
// resolvers atomically, alongside an intentional bump of the C5
// systemPromptSHA256Baseline. Until then, the new constants are pure
// dead code.

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestValidator_PlatformEntityTypesAreInTypesGo is the compile-time
// guard that the constants exist with the expected string values.
// Catches a future rename ("zone" → "region") that would silently
// break planner output produced under Phase B.
func TestValidator_PlatformEntityTypesAreInTypesGo(t *testing.T) {
	expected := map[TargetRefType]string{
		TargetRefZone:     "zone",
		TargetRefImage:    "image",
		TargetRefGPUModel: "gpu_model",
	}
	for got, want := range expected {
		assert.Equal(t, want, string(got),
			"TargetRefType %v string value drifted from contract", got)
	}
}

// TestValidator_PhaseA_RejectsNewPlatformEntityTypes pins the Phase A
// strict-zero-behavior contract: the new types remain rejected by
// validateTargetRef, identical to any unknown TargetRefType.
//
// If a future PR adds a switch case for these types, this test fails
// — Phase B must update the assertions here in the same commit as the
// validator change, alongside the planner-prompt hash bump (C5).
func TestValidator_PhaseA_RejectsNewPlatformEntityTypes(t *testing.T) {
	cases := []struct {
		name    string
		refType TargetRefType
	}{
		{"zone", TargetRefZone},
		{"image", TargetRefImage},
		{"gpu_model", TargetRefGPUModel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref := TargetRef{
				Type:       tc.refType,
				Value:      "any-value",
				Source:     SourceUserText,
				SourceSpan: "any-value",
			}
			err := validateTargetRef(ref, 0, ValidationContext{UserText: "any-value"})
			assert.Error(t, err,
				"Phase A strict contract: %s target_ref must still be rejected by validator. "+
					"If you intend to accept it, this is a Phase B change — update planner "+
					"prompt + resolvers + this test atomically.",
				tc.refType)
			if err != nil {
				assert.Contains(t, err.Error(), "unsupported target_ref type",
					"rejection message must match the unknown-type default branch")
			}
		})
	}
}
