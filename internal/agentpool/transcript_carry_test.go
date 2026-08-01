package agentpool

import (
	"testing"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/store"
)

const sampleTranscript = `{"agent_transcript_v1":{"v":1,"messages":[` +
	`{"role":"user","content":"q"},` +
	`{"role":"assistant","tool_calls":[{"id":"c1","name":"T","arguments":"{}"}]},` +
	`{"role":"tool","tool_call_id":"c1","name":"T","content":"r"},` +
	`{"role":"assistant","content":"a"}]}}`

// Both cold-rebuild paths flatten store.Message down to the engine's history
// type. That flattening is where the transcript was being dropped, so each path
// needs its own assertion — they are separate functions and one can be fixed
// while the other silently keeps discarding.
func TestFilterHistoryCarriesAssistantTranscript(t *testing.T) {
	history := filterHistory([]store.Message{
		{Role: "user", Status: "ok", Content: "q"},
		{Role: "assistant", Status: "ok", Content: "a", Metadata: []byte(sampleTranscript)},
	})
	if len(history) != 2 {
		t.Fatalf("kept %d messages, want 2", len(history))
	}
	if len(history[0].Transcript) != 0 {
		t.Fatal("user rows must not carry a transcript")
	}
	if got := engine.ParseTranscriptMetadata(history[1].Transcript); got == nil {
		t.Fatalf("assistant transcript did not survive: %q", history[1].Transcript)
	}
}

func TestValidateCommittedTailCarriesAssistantTranscript(t *testing.T) {
	history, err := validateCommittedTail([]store.Message{
		{Role: "user", Status: "ok", Content: "q"},
		{Role: "assistant", Status: "ok", Content: "a", Metadata: []byte(sampleTranscript)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := engine.ParseTranscriptMetadata(history[1].Transcript); got == nil {
		t.Fatalf("assistant transcript did not survive the durable tail: %q", history[1].Transcript)
	}
}

// Rows written before the shadow write existed have NULL metadata. They must
// rebuild exactly as they always did.
func TestRowsWithoutTranscriptRebuildUnchanged(t *testing.T) {
	history := filterHistory([]store.Message{
		{Role: "user", Status: "ok", Content: "q"},
		{Role: "assistant", Status: "ok", Content: "a"},
	})
	for _, msg := range history {
		if len(msg.Transcript) != 0 {
			t.Fatalf("invented a transcript for a legacy row: %+v", msg)
		}
		if engine.ParseTranscriptMetadata(msg.Transcript) != nil {
			t.Fatal("legacy row parsed to a non-nil transcript")
		}
	}
}
