package httpapi

import (
	"strings"
	"testing"

	"github.com/compshare-agent/internal/tools"
)

// This pins the RUNNING-ACTIVITY label, not the authorization card — the card is
// covered by TestLaneConfirmCardFollowsTheWriteGate. The original version of this
// comment said "the sentence on the card the user clicks", and believing it is why
// the card went unfixed: making this test green read as having fixed consent too.
func TestInstanceOpsLabelFollowsTheWriteGate(t *testing.T) {
	defer tools.SetInstanceOpsWritesEnabled(tools.InstanceOpsWritesEnabled())

	tools.SetInstanceOpsWritesEnabled(false)
	ro := stepActionLabel("DiagnoseInstanceInternals")
	if ro != "实例内只读排查" {
		t.Fatalf("read-only label = %q, want 实例内只读排查", ro)
	}

	tools.SetInstanceOpsWritesEnabled(true)
	rw := stepActionLabel("DiagnoseInstanceInternals")
	if rw == ro {
		t.Fatal("label did not change with the write gate: the card still says 只读 while the lane writes")
	}
	// Whatever the wording, it must not claim read-only.
	if rw == "" {
		t.Fatal("write-mode label is empty; the console would fall back to the raw tool name")
	}
	if containsReadOnly(rw) {
		t.Fatalf("write-mode label still claims read-only: %q", rw)
	}

	// Unrelated labels must not move with this flag.
	if got := stepActionLabel("DiagnoseBilling"); got != "诊断扣费异常" {
		t.Fatalf("unrelated label changed with the write gate: %q", got)
	}
}

func containsReadOnly(s string) bool { return strings.Contains(s, "只读") }
