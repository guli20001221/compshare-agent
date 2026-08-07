package observability

import (
	"encoding/json"
	"testing"
)

func TestTurnCompletionTraceMarshalWiring(t *testing.T) {
	record := TraceRecord{
		SchemaVersion: SchemaVersion,
		Completion: TurnCompletionTrace{
			Class:      CompletionClassStructuredClarify,
			Reason:     CompletionReasonContextClarification,
			ModelCalls: 2,
			ToolNames:  []string{"DescribeCompShareInstance"},
		},
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded struct {
		Completion TurnCompletionTrace `json:"completion"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Completion.Class != CompletionClassStructuredClarify || decoded.Completion.ModelCalls != 2 {
		t.Fatalf("completion lost from custom TraceRecord marshal: %#v", decoded.Completion)
	}
	if len(decoded.Completion.ToolNames) != 1 {
		t.Fatalf("tool names lost from completion: %#v", decoded.Completion)
	}
}

func TestTurnCompletionTraceZeroValueIsOmitted(t *testing.T) {
	data, err := json.Marshal(TraceRecord{SchemaVersion: SchemaVersion})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, exists := decoded["completion"]; exists {
		t.Fatal("zero completion must remain omitted for raw fixtures")
	}
}
