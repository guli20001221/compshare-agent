package httpapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/tools"
)

// The card is the door. It is what the user reads before letting us onto their
// machine, and it is NOT the step label — conflating the two is exactly how this
// shipped wrong: the step label was made write-aware, that read as having fixed
// the card, and the card kept saying 只读排查 while the harness was authorized to
// write. A client-side map keyed on the action name can never get this right,
// because the wording depends on boot state the browser cannot see.
func TestLaneConfirmCardFollowsTheWriteGate(t *testing.T) {
	defer tools.SetInstanceOpsWritesEnabled(tools.InstanceOpsWritesEnabled())

	tools.SetInstanceOpsWritesEnabled(false)
	ro := serverOwnedConfirmLabel("DiagnoseInstanceInternals")
	// Byte-identical to the console's own CONFIRM_LABELS entry, so read-only
	// deployments render exactly what they render today.
	if ro != "进入实例只读排查" {
		t.Fatalf("read-only card = %q, want 进入实例只读排查", ro)
	}

	tools.SetInstanceOpsWritesEnabled(true)
	rw := serverOwnedConfirmLabel("DiagnoseInstanceInternals")
	if rw == ro {
		t.Fatal("card title did not move with the write gate — the user approves 只读排查 and the harness then writes")
	}
	if strings.Contains(rw, "只读") {
		t.Fatalf("write-mode card still claims read-only: %q", rw)
	}
}

// The per-write card had no console entry at all, so on a real repair run the
// user was shown a card titled `InstanceOpsWriteCommand`. A gate nobody can read
// is not a gate.
func TestPerWriteConfirmCardIsNeverTheRawActionName(t *testing.T) {
	defer tools.SetInstanceOpsWritesEnabled(tools.InstanceOpsWritesEnabled())
	tools.SetInstanceOpsWritesEnabled(true)

	got := serverOwnedConfirmLabel("InstanceOpsWriteCommand")
	if got == "" || got == "InstanceOpsWriteCommand" {
		t.Fatalf("per-write card = %q; the console has no entry for this action, so the server must supply one", got)
	}
	// Distinct from the door card on purpose: a user shown the same sentence twice
	// cannot tell which of the two questions they just answered.
	if got == serverOwnedConfirmLabel("DiagnoseInstanceInternals") {
		t.Fatalf("door card and per-command card both say %q", got)
	}
}

// Workflows keep their console-side titles and their frames keep their exact
// bytes — TestConfirmationEvent_LegacyWireShapeUnchanged pins that on purpose,
// and a card label is not a good enough reason to break it.
func TestWorkflowConfirmFramesGainNoLabelKey(t *testing.T) {
	defer tools.SetInstanceOpsWritesEnabled(tools.InstanceOpsWritesEnabled())
	tools.SetInstanceOpsWritesEnabled(true) // even with writes on

	for _, action := range []string{"CreateInstanceWorkflow", "StopInstanceWorkflow", "ResetPasswordWorkflow", "DiagnoseBilling"} {
		if got := serverOwnedConfirmLabel(action); got != "" {
			t.Fatalf("%s: server sent %q; the console already titles this correctly and the frame must stay byte-identical", action, got)
		}
	}

	raw, err := json.Marshal(confirmationEvent{
		ConfirmationID: "c-1",
		Action:         "CreateInstanceWorkflow",
		Summary:        map[string]any{"GpuType": "4090"},
		TimeoutSeconds: 60,
		Label:          serverOwnedConfirmLabel("CreateInstanceWorkflow"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"Label"`) {
		t.Fatalf("workflow confirmation frame gained a Label key: %s", raw)
	}
}

// The two in-instance frames must actually carry the key — omitempty means an
// empty label is indistinguishable from "field not implemented", and the console
// would silently fall back to its stale map.
func TestInstanceOpsConfirmFramesCarryTheLabelKey(t *testing.T) {
	defer tools.SetInstanceOpsWritesEnabled(tools.InstanceOpsWritesEnabled())
	tools.SetInstanceOpsWritesEnabled(true)

	for _, action := range []string{"DiagnoseInstanceInternals", "InstanceOpsWriteCommand"} {
		raw, err := json.Marshal(confirmationEvent{
			ConfirmationID: "c-1",
			Action:         action,
			TimeoutSeconds: 60,
			Label:          serverOwnedConfirmLabel(action),
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"Label":"`) {
			t.Fatalf("%s frame has no Label key: %s", action, raw)
		}
	}
}
