package httpapi

import (
	"strings"
	"testing"
)

// This pins the RUNNING-ACTIVITY label, not the authorization card — the card is
// covered by TestLaneConfirmCardFollowsTheWriteGate. The original version of this
// comment said "the sentence on the card the user clicks", and believing it is why
// the card went unfixed: making this test green read as having fixed consent too.
func TestInstanceOpsLabelStatesTheSingleRepairContract(t *testing.T) {
	got := stepActionLabel("DiagnoseInstanceInternals")
	if got != "实例内排查与修复" {
		t.Fatalf("instance-ops label = %q, want 实例内排查与修复", got)
	}
	if containsReadOnly(got) {
		t.Fatalf("single-mode label still claims read-only: %q", got)
	}

	// Unrelated labels must not move with the instance-ops contract.
	if got := stepActionLabel("DiagnoseBilling"); got != "诊断扣费异常" {
		t.Fatalf("unrelated label changed with the write gate: %q", got)
	}
}

func containsReadOnly(s string) bool { return strings.Contains(s, "只读") }
