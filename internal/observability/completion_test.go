package observability

import (
	"encoding/json"
	"testing"
)

func TestTurnCompletionTraceMarshalWiring(t *testing.T) {
	firstChunkBelowOneMS := int64(0)
	record := TraceRecord{
		SchemaVersion: SchemaVersion,
		Completion: TurnCompletionTrace{
			Class:                 CompletionClassStructuredClarify,
			Reason:                CompletionReasonContextClarification,
			RuntimeFinishReason:   "budget_recovery",
			ModelCalls:            2,
			ModelProvider:         "modelverse",
			ModelIDs:              []string{"gpt-5.6-terra"},
			ProviderFinishReasons: []string{"tool_calls", "stop"},
			ModelAttempts: []ModelAttemptTrace{
				{AttemptInCall: 1, LatencyMS: 500, Outcome: "error", ErrorClass: "network", Retried: true},
				{AttemptInCall: 2, LatencyMS: 800, Outcome: "success", FinishReason: "stop", FirstChunkMS: &firstChunkBelowOneMS},
			},
			ToolNames: []string{"DescribeCompShareInstance"},
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
	if decoded.Completion.RuntimeFinishReason != "budget_recovery" {
		t.Fatalf("runtime finish reason lost from completion: %#v", decoded.Completion)
	}
	if len(decoded.Completion.ToolNames) != 1 {
		t.Fatalf("tool names lost from completion: %#v", decoded.Completion)
	}
	if decoded.Completion.ModelProvider != "modelverse" || len(decoded.Completion.ModelIDs) != 1 || len(decoded.Completion.ProviderFinishReasons) != 2 {
		t.Fatalf("model attribution lost from completion: %#v", decoded.Completion)
	}
	if len(decoded.Completion.ModelAttempts) != 2 || decoded.Completion.ModelAttempts[0].AttemptInCall != 1 ||
		decoded.Completion.ModelAttempts[0].FirstChunkMS != nil {
		t.Fatalf("failed attempt or absent first-chunk signal lost: %#v", decoded.Completion.ModelAttempts)
	}
	if decoded.Completion.ModelAttempts[1].FirstChunkMS == nil || *decoded.Completion.ModelAttempts[1].FirstChunkMS != 0 {
		t.Fatalf("a real sub-millisecond first chunk must survive as pointer-to-zero: %#v", decoded.Completion.ModelAttempts[1])
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
