package observability

import (
	"encoding/json"
	"testing"
)

func TestTurnCompletionTraceMarshalWiring(t *testing.T) {
	firstChunkBelowOneMS := int64(0)
	promptTokens := 120
	cachedPromptTokens := 0
	record := TraceRecord{
		SchemaVersion: SchemaVersion,
		Completion: TurnCompletionTrace{
			Class:               CompletionClassStructuredClarify,
			Reason:              CompletionReasonContextClarification,
			RuntimeFinishReason: "budget_recovery",
			ModelAttempts: []ModelAttemptTrace{
				{
					ID: "model-1", Provider: "modelverse", Model: "gpt-5.6-terra",
					AttemptInCall: 1, LatencyMS: 500, Outcome: "error", ErrorClass: "network", Retried: true,
				},
				{
					ID: "model-2", Provider: "modelverse", Model: "gpt-5.6-terra",
					AttemptInCall: 2, LatencyMS: 800, Outcome: "success", FinishReason: "stop", FirstChunkMS: &firstChunkBelowOneMS,
					PromptTokens: &promptTokens, CachedPromptTokens: &cachedPromptTokens,
					ToolCount: 2, ToolWindowRunes: 321, ToolWindowHash: "sha256:tool-window",
				},
			},
			DirectAnswerRetryOutcome: DirectAnswerRetryOutcomeToolSelected,
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
	if decoded.Completion.Class != CompletionClassStructuredClarify || len(decoded.Completion.ModelAttempts) != 2 {
		t.Fatalf("completion lost from custom TraceRecord marshal: %#v", decoded.Completion)
	}
	if decoded.Completion.RuntimeFinishReason != "budget_recovery" {
		t.Fatalf("runtime finish reason lost from completion: %#v", decoded.Completion)
	}
	if decoded.Completion.DirectAnswerRetryOutcome != DirectAnswerRetryOutcomeToolSelected {
		t.Fatalf("direct-answer retry outcome lost from completion: %#v", decoded.Completion)
	}
	if len(decoded.Completion.ModelAttempts) != 2 || decoded.Completion.ModelAttempts[0].AttemptInCall != 1 ||
		decoded.Completion.ModelAttempts[0].FirstChunkMS != nil {
		t.Fatalf("failed attempt or absent first-chunk signal lost: %#v", decoded.Completion.ModelAttempts)
	}
	for i, attempt := range decoded.Completion.ModelAttempts {
		wantID := "model-1"
		if i == 1 {
			wantID = "model-2"
		}
		if attempt.ID != wantID || attempt.Provider != "modelverse" || attempt.Model != "gpt-5.6-terra" {
			t.Fatalf("typed model operation %d lost identity: %#v", i, attempt)
		}
	}
	if decoded.Completion.ModelAttempts[1].FirstChunkMS == nil || *decoded.Completion.ModelAttempts[1].FirstChunkMS != 0 {
		t.Fatalf("a real sub-millisecond first chunk must survive as pointer-to-zero: %#v", decoded.Completion.ModelAttempts[1])
	}
	secondAttempt := decoded.Completion.ModelAttempts[1]
	if secondAttempt.PromptTokens == nil || *secondAttempt.PromptTokens != 120 ||
		secondAttempt.CachedPromptTokens == nil || *secondAttempt.CachedPromptTokens != 0 {
		t.Fatalf("missing usage and a real zero cache hit must remain distinct: %#v", secondAttempt)
	}
	if secondAttempt.ToolCount != 2 || secondAttempt.ToolWindowRunes != 321 || secondAttempt.ToolWindowHash != "sha256:tool-window" {
		t.Fatalf("tool-window observation lost from attempt: %#v", secondAttempt)
	}
	var wire struct {
		Completion struct {
			ModelAttempts []map[string]json.RawMessage `json:"model_attempts"`
		} `json:"completion"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("Unmarshal wire shape: %v", err)
	}
	if _, ok := wire.Completion.ModelAttempts[0]["tool_count"]; !ok {
		t.Fatal("a measured zero tool count must remain explicit")
	}
	if _, ok := wire.Completion.ModelAttempts[0]["tool_window_runes"]; !ok {
		t.Fatal("a measured zero tool-window size must remain explicit")
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
