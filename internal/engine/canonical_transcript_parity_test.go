package engine

import (
	"reflect"
	"testing"

	openai "github.com/sashabaranov/go-openai"
)

// hotTurn is the message list a live engine holds for one turn.
func hotTurn() []openai.ChatCompletionMessage {
	return []openai.ChatCompletionMessage{
		userMsg("帮我看看 4090 那台是不是掉卡了"),
		assistantCalls(
			call("c1", "SearchKnowledge", `{"query":"GPU 掉卡"}`),
			call("c2", "DescribeCompShareInstance", `{"UHostId":"cpod-x"}`),
		),
		toolMsg("c1", `{"chunks":[{"id":"kb-1"}]}`),
		toolMsg("c2", `{"State":"Running","GpuCount":1}`),
		assistantCalls(call("c3", "DiagnoseInstanceInternals", `{"UHostId":"cpod-x"}`)),
		toolMsg("c3", "nvidia-smi 正常，驱动 570.169"),
		finalMsg("没有掉卡。"),
	}
}

// The whole point of persisting the transcript is that a cold rebuild produces
// the same messages the hot engine would have replayed. If the two diverge, a
// session behaves differently after a restart than before it, which is the
// amnesia this work exists to remove.
//
// Note what this does NOT claim: that either side equals the live turn
// byte-for-byte. Both sides are redacted, both are truncated at the same
// bounds, and both shed the same rounds — parity is between hot replay and cold
// replay, not between replay and the original. The fixture below is deliberately
// secret-free and under every budget so those transforms are no-ops here and the
// test isolates the rebuild. Adding a credential or an over-long body to it
// would not be a stronger test; it would assert a property the design rejects.
func TestHotAndColdProduceIdenticalTurnMessages(t *testing.T) {
	hot := hotTurn()

	// Hot side: what the live engine holds and would have sent.
	e := &Engine{messages: hot}
	e.captureTurnTranscript()
	persisted, stats := e.LastTurnTranscript()
	if persisted == nil {
		t.Fatal("turn produced no transcript to persist")
	}
	if stats.Dropped != 0 {
		t.Fatalf("this fixture must fit the budget; dropped %d rounds", stats.Dropped)
	}

	// Cold side: rebuild from the persisted row alone.
	cold := ProjectTranscript(ParseTranscriptMetadata(persisted))

	if !reflect.DeepEqual(cold, hot) {
		t.Fatalf("hot/cold divergence\n hot: %#v\ncold: %#v", hot, cold)
	}
}

// A row written before the shadow write existed, or by a store that could not
// write metadata, must degrade to "no transcript" — never to a failed rebuild.
func TestParseTranscriptMetadataDegradesOnUnusableInput(t *testing.T) {
	for name, raw := range map[string]string{
		"empty":            ``,
		"not json":         `{oh no`,
		"unrelated object": `{"something_else":1}`,
		"null transcript":  `{"agent_transcript_v1":null}`,
		"future version":   `{"agent_transcript_v1":{"v":99,"messages":[{"role":"user"}]}}`,
		"no messages":      `{"agent_transcript_v1":{"v":1,"messages":[]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if got := ParseTranscriptMetadata([]byte(raw)); got != nil {
				t.Fatalf("expected nil for %q, got %+v", raw, got)
			}
			// And the projector must tolerate that nil.
			if got := ProjectTranscript(nil); got != nil {
				t.Fatalf("projecting nil returned %+v", got)
			}
		})
	}
}

// Half a round must never reach a provider: an assistant tool_call with no
// matching result (or the reverse) is a 400 on the ENTIRE request, so it would
// take down a turn that had nothing wrong with it.
func TestProjectTranscriptDropsUnpairedToolTraffic(t *testing.T) {
	transcript := &TranscriptV1{V: 1, Messages: []TranscriptMessage{
		{Role: openai.ChatMessageRoleUser, Content: "q"},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []TranscriptToolCall{
			{ID: "answered", Name: "T"},
			{ID: "orphaned", Name: "T"},
		}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "answered", Content: "r"},
		// A result whose call is absent entirely.
		{Role: openai.ChatMessageRoleTool, ToolCallID: "never-declared", Content: "r"},
		{Role: openai.ChatMessageRoleAssistant, Content: "a"},
	}}

	projected := ProjectTranscript(transcript)

	declared := map[string]bool{}
	for _, msg := range projected {
		for _, one := range msg.ToolCalls {
			declared[one.ID] = false
		}
	}
	if _, ok := declared["orphaned"]; ok {
		t.Fatal("kept a tool_call with no result")
	}
	for _, msg := range projected {
		if msg.Role != openai.ChatMessageRoleTool {
			continue
		}
		if _, ok := declared[msg.ToolCallID]; !ok {
			t.Fatalf("kept tool result %q whose call is absent", msg.ToolCallID)
		}
		declared[msg.ToolCallID] = true
	}
	for id, answered := range declared {
		if !answered {
			t.Fatalf("tool_call %q survived unanswered", id)
		}
	}
}

// An assistant message that was nothing but unanswered tool_calls carries no
// information once they are dropped, and an empty assistant message is not
// valid provider input.
func TestProjectTranscriptDropsEmptiedAssistantMessages(t *testing.T) {
	projected := ProjectTranscript(&TranscriptV1{V: 1, Messages: []TranscriptMessage{
		{Role: openai.ChatMessageRoleUser, Content: "q"},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []TranscriptToolCall{{ID: "orphaned", Name: "T"}}},
		{Role: openai.ChatMessageRoleAssistant, Content: "a"},
	}})
	for _, msg := range projected {
		if msg.Role == openai.ChatMessageRoleAssistant && msg.Content == "" && len(msg.ToolCalls) == 0 {
			t.Fatalf("kept an empty assistant message: %#v", projected)
		}
	}
	if len(projected) != 2 {
		t.Fatalf("projected %d messages, want user + final assistant: %#v", len(projected), projected)
	}
}

// When bounding shed rounds, hot and cold diverge BY DESIGN — cold holds less.
// What must survive is well-formedness: the question, the answer, and only
// complete rounds in between.
func TestShedTurnStaysWellFormedAfterRoundTrip(t *testing.T) {
	big := make([]rune, maxTranscriptMessageRunes)
	for i := range big {
		big[i] = '据'
	}
	messages := []openai.ChatCompletionMessage{userMsg("q")}
	for i := 0; i < 12; i++ {
		id := string(rune('a' + i))
		messages = append(messages, assistantCalls(call(id, "Tool", `{}`)), toolMsg(id, string(big)))
	}
	messages = append(messages, finalMsg("答案"))

	e := &Engine{messages: messages}
	e.captureTurnTranscript()
	persisted, stats := e.LastTurnTranscript()
	if persisted == nil || stats.Dropped == 0 {
		t.Fatalf("fixture should have shed rounds; stats = %+v", stats)
	}

	cold := ProjectTranscript(ParseTranscriptMetadata(persisted))
	if len(cold) >= len(messages) {
		t.Fatalf("shed turn projected %d messages, want fewer than %d", len(cold), len(messages))
	}
	if cold[0].Content != "q" {
		t.Fatalf("question did not survive: %#v", cold[0])
	}
	if last := cold[len(cold)-1]; last.Content != "答案" {
		t.Fatalf("answer did not survive: %#v", last)
	}
	// Every surviving round is still complete.
	open := map[string]bool{}
	for _, msg := range cold {
		for _, one := range msg.ToolCalls {
			open[one.ID] = true
		}
		if msg.Role == openai.ChatMessageRoleTool {
			if !open[msg.ToolCallID] {
				t.Fatalf("orphan tool result %q survived shedding", msg.ToolCallID)
			}
			delete(open, msg.ToolCallID)
		}
	}
	if len(open) != 0 {
		t.Fatalf("tool_calls left unanswered after shedding: %v", open)
	}
}
