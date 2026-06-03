package ws

import (
	"encoding/json"
	"testing"
)

// TestBuildFrame_InjectsEventAndFlattens verifies the WS frame contract that the
// frontend depends on: a single JSON object with an "event" discriminator (the
// key service.js switches on) and the payload's fields flattened to the top
// level (not nested). Getting either wrong silently breaks the frontend's frame
// routing.
func TestBuildFrame_InjectsEventAndFlattens(t *testing.T) {
	type token struct {
		Text string `json:"Text"`
	}
	raw, err := buildFrame("token", token{Text: "hello"})
	if err != nil {
		t.Fatalf("buildFrame: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["event"] != "token" {
		t.Errorf("event = %v, want token", got["event"])
	}
	if got["Text"] != "hello" {
		t.Errorf("Text = %v, want hello (payload must be flattened, not nested)", got["Text"])
	}
	if _, nested := got["data"]; nested {
		t.Errorf("frame must not nest payload under a data key: %s", raw)
	}
}

// TestBuildFrame_NilData produces an event-only frame — used for frames like
// "done" that may carry no payload.
func TestBuildFrame_NilData(t *testing.T) {
	raw, err := buildFrame("done", nil)
	if err != nil {
		t.Fatalf("buildFrame: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["event"] != "done" {
		t.Errorf("event = %v, want done", got["event"])
	}
	if len(got) != 1 {
		t.Errorf("nil-data frame should carry only event, got %v", got)
	}
}

// TestBuildFrame_EventWinsOnCollision ensures a payload field named "event"
// cannot shadow the frame discriminator — the transport tag must be authoritative.
func TestBuildFrame_EventWinsOnCollision(t *testing.T) {
	raw, err := buildFrame("step", map[string]any{"event": "attacker", "Index": 3})
	if err != nil {
		t.Fatalf("buildFrame: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["event"] != "step" {
		t.Errorf("event = %v, want step (discriminator must win over payload)", got["event"])
	}
}
