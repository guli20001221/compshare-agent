package engine

import (
	"github.com/stretchr/testify/require"
	"reflect"
	"testing"

	openai "github.com/sashabaranov/go-openai"
)

// finishTurn runs a completed turn through the engine's capture path, the same
// way ChatWithOptions' deferred hook does.
func finishTurn(e *Engine, turn []openai.ChatCompletionMessage) {
	e.messages = append(e.messages, turn...)
	e.captureTurnTranscript()
	e.messages = stripHistoricalToolTranscript(e.messages)
}

// The next turn sees what the previous turn actually did, not a prose summary.
func TestPriorTurnToolTrafficReachesTheModel(t *testing.T) {
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
	// Keep the fixture shaped like a real replayed exchange: the transcript must
	// contain the exact user and final assistant messages plus a valid tool round.
	// A tool-only slice is not a representation messagesFromAgentContext may use.
	call := toolCall("c1", "Read", `{}`)
	big := make([]rune, maxReplayedHistoryRunes-len([]rune("q"))-len([]rune("a"))-len([]rune(call.Function.Name))-len([]rune(call.Function.Arguments)))
	for i := range big {
		big[i] = '据'
	}
	heavy := ConversationPair{
		User:      "q",
		Assistant: "a",
		Transcript: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleUser, Content: "q"},
			{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{call}},
			{Role: openai.ChatMessageRoleTool, Content: string(big), ToolCallID: "c1"},
			{Role: openai.ChatMessageRoleAssistant, Content: "a"},
		},
	}
	if got := conversationTranscriptRunes(heavy); got != maxReplayedHistoryRunes {
		t.Fatalf("transcript cost = %d, want %d", got, maxReplayedHistoryRunes)
	}

	kept := budgetReplayedPairs([]ConversationPair{
		heavy,
		{User: "newer detailed q", Assistant: "newer detailed a", Transcript: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleUser, Content: "newer detailed q"},
			{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{toolCall("c2", "Read", `{}`)}},
			{Role: openai.ChatMessageRoleTool, ToolCallID: "c2", Content: "fresh evidence"},
			{Role: openai.ChatMessageRoleAssistant, Content: "newer detailed a"},
		}},
		{User: "new q", Assistant: "new a"},
	}, maxReplayedHistoryRunes)

	require.Len(t, kept, 3, "plain user/assistant dialogue must survive detail compaction")
	require.Empty(t, kept[0].Transcript, "the oversized old tool detail must not escape the budget")
	require.NotEmpty(t, kept[1].Transcript, "the newest available tool evidence wins")
	require.Equal(t, "q", kept[0].User)
	require.Equal(t, "new q", kept[len(kept)-1].User)
}
