package server

import (
	"testing"

	"github.com/compshare-agent/internal/engine"
)

// TestStepEventToServerMessage_PerToolErrorIsToolResult locks in the
// PR7 protocol fix: when the engine emits a StepError (a single tool
// call failed mid-turn), the WS server MUST translate it to
// tool_result+OK=false, NOT a top-level error frame. Encodes WHY:
//
//   - Real prod scenario (verified 2026-05-21 in §14 smoke): the
//     upstream API returned RetCode=230 on CheckCompShareResourceCapacity
//     for an unavailable zone. The engine recovered, retried other tools,
//     and produced a partial answer. A naïve client that bails on
//     {"type":"error"} would have closed the connection early and missed
//     the answer.
//   - error frames are reserved for whole-turn failures (chatErr != nil
//     at runChatTurn) so clients have one unambiguous "stop listening"
//     signal.
func TestStepEventToServerMessage_PerToolErrorIsToolResult(t *testing.T) {
	ev := engine.StepEvent{
		Type:    engine.StepError,
		Action:  "CheckCompShareResourceCapacity",
		Message: "API error (RetCode=230): Params [Zone] not available",
	}
	got, ok := stepEventToServerMessage(ev, "req-uuid-abc")
	if !ok {
		t.Fatalf("StepError event must produce a server message, got ok=false")
	}
	if got.Type != ServerMsgToolResult {
		t.Fatalf("StepError must emit tool_result, got %q", got.Type)
	}
	if got.OK == nil || *got.OK != false {
		t.Fatalf("per-tool error must carry OK=false, got %+v", got.OK)
	}
	if got.Action != "CheckCompShareResourceCapacity" {
		t.Fatalf("Action drift: got %q", got.Action)
	}
	if got.RequestUUID != "req-uuid-abc" {
		t.Fatalf("RequestUUID drift: got %q", got.RequestUUID)
	}
	if got.Message != "API error (RetCode=230): Params [Zone] not available" {
		t.Fatalf("error Message must be surfaced verbatim, got %q", got.Message)
	}
	if got.Code != "" {
		t.Errorf("tool_result frame must not carry a top-level error Code; "+
			"got Code=%q which would confuse clients into thinking the turn failed",
			got.Code)
	}
}

// TestStepEventToServerMessage_StepBlockedSameShape — StepBlocked
// (e.g. mutating-tools-disabled refusal) and StepError now share the
// same wire shape (tool_result+OK=false). Asserts the parity so
// clients only need one branch.
func TestStepEventToServerMessage_StepBlockedSameShape(t *testing.T) {
	ev := engine.StepEvent{
		Type:    engine.StepBlocked,
		Action:  "StopCompShareInstance",
		Message: "mutating tools disabled in this deployment",
	}
	got, ok := stepEventToServerMessage(ev, "req-uuid-blocked")
	if !ok || got.Type != ServerMsgToolResult {
		t.Fatalf("StepBlocked must emit tool_result, got %+v ok=%v", got, ok)
	}
	if got.OK == nil || *got.OK != false {
		t.Fatalf("blocked must carry OK=false")
	}
}

// TestStepEventToServerMessage_ToolCallAndSuccess covers the happy
// paths so the refactor (handler.go onStep extraction) is locked in
// against accidental shape drift.
func TestStepEventToServerMessage_ToolCallAndSuccess(t *testing.T) {
	cases := []struct {
		name       string
		ev         engine.StepEvent
		wantType   string
		wantOK     *bool
		wantAction string
	}{
		{
			name:       "tool call",
			ev:         engine.StepEvent{Type: engine.StepToolCall, Action: "DescribeCompShareInstance"},
			wantType:   ServerMsgToolCall,
			wantOK:     nil,
			wantAction: "DescribeCompShareInstance",
		},
		{
			name:       "tool success",
			ev:         engine.StepEvent{Type: engine.StepToolResult, Action: "DescribeCompShareInstance"},
			wantType:   ServerMsgToolResult,
			wantOK:     boolPtr(true),
			wantAction: "DescribeCompShareInstance",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := stepEventToServerMessage(c.ev, "uuid")
			if !ok {
				t.Fatalf("expected projection, got ok=false")
			}
			if got.Type != c.wantType {
				t.Errorf("Type drift: got %q want %q", got.Type, c.wantType)
			}
			if got.Action != c.wantAction {
				t.Errorf("Action drift: got %q want %q", got.Action, c.wantAction)
			}
			switch {
			case c.wantOK == nil && got.OK != nil:
				t.Errorf("expected OK=nil, got %v", *got.OK)
			case c.wantOK != nil && (got.OK == nil || *got.OK != *c.wantOK):
				t.Errorf("OK drift: got %v want %v", got.OK, *c.wantOK)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }
