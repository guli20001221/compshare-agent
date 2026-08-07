package engine

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"reflect"
	"testing"

	openai "github.com/sashabaranov/go-openai"
)

func withCanonicalTranscript(t *testing.T, enabled bool) {
	t.Helper()
	prev := canonicalTranscriptEnabled
	canonicalTranscriptEnabled = enabled
	t.Cleanup(func() { canonicalTranscriptEnabled = prev })
}

// finishTurn runs a completed turn through the engine's capture path, the same
// way ChatWithOptions' deferred hook does.
func finishTurn(e *Engine, turn []openai.ChatCompletionMessage) {
	e.messages = append(e.messages, turn...)
	e.captureTurnTranscript()
	e.messages = stripHistoricalToolTranscript(e.messages)
}

// The flag is the whole safety story for this change. With it off, nothing the
// model reads may differ by a single byte.
func TestFlagOffLeavesModelHistoryByteIdentical(t *testing.T) {
	build := func(enabled bool) []openai.ChatCompletionMessage {
		withCanonicalTranscript(t, enabled)
		e := &Engine{messages: []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "sys"}}}
		finishTurn(e, hotTurn())
		e.messages = append(e.messages, userMsg("下一轮"))
		pairs := e.recentCompleteConversationPairs()
		return messagesFromAgentContext(e.messages, AgentContext{RecentConversation: pairs}, true)
	}

	off := build(false)
	for _, msg := range off {
		if msg.Role == openai.ChatMessageRoleTool || len(msg.ToolCalls) > 0 {
			t.Fatalf("flag off must not replay tool traffic: %#v", msg)
		}
	}

	if on := build(true); reflect.DeepEqual(on, off) {
		t.Fatal("flag on produced identical history — the projection is not wired")
	}
}

// The payoff: with the flag on, the next turn sees what the previous turn
// actually did, not a prose summary of it.
func TestPriorTurnToolTrafficReachesTheModel(t *testing.T) {
	enableCanonicalTranscriptForTest(t)
	withCanonicalTranscript(t, true)
	e := &Engine{messages: []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "sys"}}}
	finishTurn(e, hotTurn())
	// Turn two begins. This is the whole question: does the next turn see what
	// the previous one actually did?
	e.messages = append(e.messages, userMsg("那再帮我看看磁盘"))

	pairs := e.recentCompleteConversationPairs()
	assembled := messagesFromAgentContext(e.messages, AgentContext{RecentConversation: pairs}, true)

	var sawCall, sawResult bool
	for _, msg := range assembled {
		for _, one := range msg.ToolCalls {
			if one.Function.Name == "DiagnoseInstanceInternals" {
				sawCall = true
			}
		}
		if msg.Role == openai.ChatMessageRoleTool && msg.Content == "nvidia-smi 正常，驱动 570.169" {
			sawResult = true
		}
	}
	if !sawCall || !sawResult {
		t.Fatalf("prior turn's tool call/result missing (call=%v result=%v)", sawCall, sawResult)
	}

	// The question must appear exactly once. The transcript already opens with
	// it, so also emitting the plain pair would send it twice — and a model
	// reading the same question twice has been told it was asked twice.
	question := hotTurn()[0].Content
	count := 0
	for _, msg := range assembled {
		if msg.Role == openai.ChatMessageRoleUser && msg.Content == question {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("question replayed %d times, want exactly 1", count)
	}
}

// A hot engine and a cold rebuild of the same session must give the model the
// same history. This is the claim the whole migration rests on, asserted across
// a turn boundary rather than within one turn.
func TestHotAndColdAgreeOnReplayedHistory(t *testing.T) {
	enableCanonicalTranscriptForTest(t)
	withCanonicalTranscript(t, true)

	hot := &Engine{messages: []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "sys"}}}
	finishTurn(hot, hotTurn())
	persisted, _ := hot.LastTurnTranscript()
	if persisted == nil {
		t.Fatal("turn produced nothing to persist")
	}

	cold := &Engine{}
	cold.RehydrateHistory([]HistoryMessage{
		{Role: openai.ChatMessageRoleUser, Content: hotTurn()[0].Content},
		{Role: openai.ChatMessageRoleAssistant, Content: "没有掉卡。", Transcript: persisted},
	})

	hotPairs := hot.recentCompleteConversationPairs()
	coldPairs := cold.recentCompleteConversationPairs()
	if !reflect.DeepEqual(hotPairs, coldPairs) {
		t.Fatalf("hot/cold replayed history diverged\n hot: %#v\ncold: %#v", hotPairs, coldPairs)
	}
	if len(hotPairs) != 1 || len(hotPairs[0].Transcript) == 0 {
		t.Fatalf("expected one exchange carrying a transcript, got %#v", hotPairs)
	}
}

// Attaching one turn's tool results to a different turn's answer would be
// invisible and would tell the model a diagnosis belonged to an instance it was
// never run against. Matching is by content for exactly that reason.
func TestTranscriptsAreNotAttachedToTheWrongExchange(t *testing.T) {
	enableCanonicalTranscriptForTest(t)
	withCanonicalTranscript(t, true)
	turnA := &TranscriptV1{V: 1, Messages: []TranscriptMessage{
		{Role: openai.ChatMessageRoleUser, Content: "问题 A"},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []TranscriptToolCall{{ID: "c1", Name: "T"}}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "c1", Content: "A 的证据"},
		{Role: openai.ChatMessageRoleAssistant, Content: "答案 A"},
	}}
	e := &Engine{recentTurns: []recordedTurn{
		{User: "问题 A", Assistant: "答案 A", Transcript: turnA},
	}}

	attached := e.attachRecordedTranscripts([]ConversationPair{
		{User: "问题 B", Assistant: "答案 B"},
	})
	if len(attached[0].Transcript) != 0 {
		t.Fatalf("attached turn A's evidence to turn B: %#v", attached[0].Transcript)
	}

	// The matching exchange still gets its own.
	attached = e.attachRecordedTranscripts([]ConversationPair{{User: "问题 A", Assistant: "答案 A"}})
	if len(attached[0].Transcript) == 0 {
		t.Fatal("matching exchange lost its transcript")
	}
}

// Tool traffic consumes the detail budget, but not at the cost of forgetting the
// actual conversation. When detail no longer fits, the old exchange remains as
// its original user/assistant pair and only the re-queryable evidence is shed.
func TestReplayBudgetCompactsOldToolTrafficBeforeDroppingDialogue(t *testing.T) {
	enableCanonicalTranscriptForTest(t)
	big := make([]rune, maxReplayedHistoryRunes)
	for i := range big {
		big[i] = '据'
	}
	heavy := ConversationPair{
		User:      "q",
		Assistant: "a",
		Transcript: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleTool, Content: string(big), ToolCallID: "c1"},
		},
	}
	if got := conversationTranscriptRunes(heavy); got != maxReplayedHistoryRunes {
		t.Fatalf("transcript cost = %d, want %d", got, maxReplayedHistoryRunes)
	}

	kept := budgetReplayedPairs([]ConversationPair{
		heavy,
		{User: "newer detailed q", Assistant: "newer detailed a", Transcript: []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleTool, Content: "fresh evidence"}}},
		{User: "new q", Assistant: "new a"},
	}, maxReplayedHistoryRunes)

	require.Len(t, kept, 3, "plain user/assistant dialogue must survive detail compaction")
	require.Empty(t, kept[0].Transcript, "the oversized old tool detail must not escape the budget")
	require.NotEmpty(t, kept[1].Transcript, "the newest available tool evidence wins")
	require.Equal(t, "q", kept[0].User)
	require.Equal(t, "new q", kept[len(kept)-1].User)
}

// TestFlagOffMeansNoTranscriptPipelineAtAll is the property the single switch
// exists to provide. For a while it did not hold: capture and the shadow write
// ran unconditionally while only the projection was gated, so deploying the code
// carried a permanent background side effect — CPU on every tool-bearing turn
// and a write to messages.metadata — with no way to stop it short of shipping a
// revert.
//
// Off must mean off: nothing scanned, nothing redacted, nothing serialized,
// nothing recorded, and nothing for the persistence path to pick up.
func TestFlagOffMeansNoTranscriptPipelineAtAll(t *testing.T) {
	prev := canonicalTranscriptEnabled
	SetCanonicalTranscriptEnabled(false)
	defer SetCanonicalTranscriptEnabled(prev)

	turn := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "查一下"},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{
			toolCall("c1", "DescribeCompShareInstance", `{"UHostId":"uhost-a"}`)}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "c1", Content: `{"RetCode":0}`},
		{Role: openai.ChatMessageRoleAssistant, Content: "已确认。"},
	}

	t.Run("hot capture records nothing", func(t *testing.T) {
		e := &Engine{messages: turn}
		e.captureTurnTranscript()

		payload, stats := e.LastTurnTranscript()
		if payload != nil {
			t.Fatalf("produced a payload with the flag off: %s", payload)
		}
		if stats.Attempted {
			t.Fatal("counted an attempt with the flag off; the persistence path would then try to write")
		}
		if len(e.recentTurns) != 0 {
			t.Fatalf("recorded %d turns with the flag off", len(e.recentTurns))
		}
	})

	t.Run("cold rebuild records nothing", func(t *testing.T) {
		e := &Engine{}
		e.RehydrateHistory([]HistoryMessage{
			{Role: openai.ChatMessageRoleUser, Content: "查一下"},
			{Role: openai.ChatMessageRoleAssistant, Content: "已确认。",
				Transcript: []byte(`{"agent_transcript_v1":{"v":1,"messages":[{"role":"user","content":"查一下"}]}}`)},
		})
		if len(e.recentTurns) != 0 {
			t.Fatalf("a restart built a %d-turn window that nothing will read", len(e.recentTurns))
		}
	})
}

// TestTranscriptFromRow_DoesNotParseWithFlagOff pins the gate that recordTurn
// cannot provide. Go evaluates a call's arguments before the call, so a flag
// check inside recordTurn leaves ParseTranscriptMetadata running on every
// assistant row of every rehydration with the flag off. The window came out
// empty either way, which is exactly what made it easy to miss.
func TestTranscriptFromRow_DoesNotParseWithFlagOff(t *testing.T) {
	valid := []byte(`{"agent_transcript_v1":{"v":1,"messages":[` +
		`{"role":"assistant","tool_calls":[{"id":"c1","name":"T","arguments":"{}"}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"r"}]}}`)

	// Precondition: this metadata really does parse, so a nil below means the
	// gate stopped it rather than the input being unusable.
	require.NotNil(t, ParseTranscriptMetadata(valid),
		"precondition: the fixture must be parseable, or this test proves nothing")

	t.Run("off", func(t *testing.T) {
		prev := canonicalTranscriptEnabled
		SetCanonicalTranscriptEnabled(false)
		defer SetCanonicalTranscriptEnabled(prev)
		assert.Nil(t, transcriptFromRow(valid),
			"with the flag off the canonical parse must not run at all")
	})

	t.Run("on", func(t *testing.T) {
		enableCanonicalTranscriptForTest(t)
		assert.NotNil(t, transcriptFromRow(valid), "and with it on the row must still rebuild")
	})
}
