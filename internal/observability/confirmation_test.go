package observability

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConfirmationOutcomeAttributionKeepsTerminalReasonsDistinct(t *testing.T) {
	cases := []struct {
		name       string
		reason     string
		terminated string
		abortCause string
		errorClass string
	}{
		{"declined", ConfirmationReasonUserDeclined, TerminatedByUserCancel, AbortCauseUserDeclined, ""},
		{"timeout", ConfirmationReasonTimeout, TerminatedByUserCancel, AbortCauseConfirmationTimeout, ""},
		{"disconnect", ConfirmationReasonClientDisconnect, TerminatedByUserCancel, AbortCauseClientDisconnect, ""},
		{"delivery failed", ConfirmationReasonDeliveryFailed, TerminatedByError, "", "confirmation_delivery_failed"},
		{"broker cancelled", ConfirmationReasonBrokerCancelled, TerminatedByError, "", "confirmation_broker_cancelled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := TraceRecord{Confirmations: []ConfirmationTrace{{
				Action:         "CreateInstanceWorkflow",
				State:          ConfirmationStateNotConfirmed,
				TerminalReason: tc.reason,
				ElapsedMS:      17,
			}}}
			record.FinalizeOutcome(FinishSignals{})
			if record.Outcome.TerminatedBy != tc.terminated {
				t.Fatalf("terminated_by = %q, want %q", record.Outcome.TerminatedBy, tc.terminated)
			}
			if record.Outcome.AbortCause != tc.abortCause {
				t.Fatalf("abort_cause = %q, want %q", record.Outcome.AbortCause, tc.abortCause)
			}
			if record.Outcome.ErrorClass != tc.errorClass {
				t.Fatalf("error_class = %q, want %q", record.Outcome.ErrorClass, tc.errorClass)
			}
		})
	}
}

func TestConfirmationOutcomeUsesOnlyTheLatestCard(t *testing.T) {
	cases := []struct {
		name          string
		confirmations []ConfirmationTrace
		terminated    string
		abortCause    string
	}{
		{
			// The turn this PR exists for: the premature completion lock used to
			// freeze the whole turn on the first card that came back
			// not-confirmed, so a later confirmed card and its committed write
			// were still reported as declined.
			name: "earlier decline then final confirmation completes",
			confirmations: []ConfirmationTrace{
				{Action: "OtherWorkflow", State: ConfirmationStateNotConfirmed, TerminalReason: ConfirmationReasonUserDeclined},
				{Action: "CreateInstanceWorkflow", State: ConfirmationStateConfirmed, TerminalReason: ConfirmationReasonUserConfirmed},
			},
			terminated: TerminatedByDone,
		},
		{
			name: "earlier timeout then final confirmation completes",
			confirmations: []ConfirmationTrace{
				{Action: "OtherWorkflow", State: ConfirmationStateNotConfirmed, TerminalReason: ConfirmationReasonTimeout},
				{Action: "CreateInstanceWorkflow", State: ConfirmationStateConfirmed, TerminalReason: ConfirmationReasonUserConfirmed},
			},
			terminated: TerminatedByDone,
		},
		{
			name: "final timeout cancels",
			confirmations: []ConfirmationTrace{
				{Action: "CreateInstanceWorkflow", State: ConfirmationStateConfirmed, TerminalReason: ConfirmationReasonUserConfirmed},
				{Action: "CreateInstanceWorkflow", State: ConfirmationStateNotConfirmed, TerminalReason: ConfirmationReasonTimeout},
			},
			terminated: TerminatedByUserCancel,
			abortCause: AbortCauseConfirmationTimeout,
		},
		{
			name: "final decline cancels",
			confirmations: []ConfirmationTrace{
				{Action: "CreateInstanceWorkflow", State: ConfirmationStateConfirmed, TerminalReason: ConfirmationReasonUserConfirmed},
				{Action: "CreateInstanceWorkflow", State: ConfirmationStateNotConfirmed, TerminalReason: ConfirmationReasonUserDeclined},
			},
			terminated: TerminatedByUserCancel,
			abortCause: AbortCauseUserDeclined,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := TraceRecord{Confirmations: tc.confirmations}
			record.FinalizeOutcome(FinishSignals{})
			if record.Outcome.TerminatedBy != tc.terminated || record.Outcome.AbortCause != tc.abortCause {
				t.Fatalf("outcome = %#v, want terminated_by=%q abort_cause=%q", record.Outcome, tc.terminated, tc.abortCause)
			}
		})
	}
}

func TestConfirmationTraceMarshalsBoundedGuidedMetadata(t *testing.T) {
	stepIndex := 6
	final := true
	record := TraceRecord{
		SchemaVersion: SchemaVersion,
		TraceID:       "trace-confirmation",
		TurnID:        "turn-confirmation",
		Confirmations: []ConfirmationTrace{{
			Action: "CreateInstanceWorkflow", State: ConfirmationStateConfirmed,
			TerminalReason: ConfirmationReasonUserConfirmed, ElapsedMS: 123,
			StepIndex: &stepIndex, StepTitle: "第六步，确认镜像与计费", Final: &final,
			ConfirmedContract: &ConfirmedCreateContract{
				GPUType: "H20", GPU: 1, CPU: 16, MemoryMB: 245760,
				Zone: "cn-wlcb-01", Image: "InfiniteTalk", ChargeType: "Postpay",
				EstimatedPrice: "¥7.12/小时（预估）",
			},
		}},
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		`"terminal_reason":"user_confirmed"`, `"elapsed_ms":123`, `"step_index":6`,
		`"final":true`, `"confirmed_contract"`, `"gpu_type":"H20"`,
		`"estimated_price":"¥7.12/小时（预估）"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("trace missing %s: %s", want, text)
		}
	}
}
