package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const shortTurnSessionPairs = 60

func shortTurnEngine(t *testing.T) *Engine {
	t.Helper()
	e := &Engine{}
	e.sessionStateHydrated = true
	e.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "system"}}
	for i := 0; i < shortTurnSessionPairs; i++ {
		e.messages = append(e.messages,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: fmt.Sprintf("问题%d", i)},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: fmt.Sprintf("回答%d", i)},
		)
	}
	return e
}

func TestReplayWindowIsLimitedBySize(t *testing.T) {
	e := shortTurnEngine(t)

	pairs := e.recentConversationPairs()

	require.Less(t, conversationPairsRunes(pairs), maxReplayedHistoryRunes,
		"premise: %d short exchanges must genuinely fit the budget, or this test is "+
			"not exercising full-session replay", shortTurnSessionPairs)
	assert.Len(t, pairs, shortTurnSessionPairs,
		"all exchanges that fit the size budget should be replayed")
	assert.Equal(t, "问题0", pairs[0].User, "including the session's opening turn")
}

func TestAssembledRequestUsesTheSizeBudget(t *testing.T) {
	e := shortTurnEngine(t)
	e.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(
		e, "问题0 说的是哪台", "t", time.Unix(1_800_000_000, 0))
	e.turnContextViewReady = true

	out := e.buildMessagesForLLM(productionToolWindow())

	require.Greater(t, len(out), 100, "premise: many short messages must fit")
	assert.LessOrEqual(t, assembledRequestRunes(out)+toolWindowRunes(productionToolWindow()),
		maxAssembledRequestRunes, "and it must still respect the one bound that remains")
	assert.Contains(t, renderTestMessages(out), "问题0",
		"the oldest exchange survived the size budget but not the assembler")
}

func TestRecordedTurnWindowUsesTheSizeBudget(t *testing.T) {

	e := &Engine{}
	for i := 0; i < shortTurnSessionPairs; i++ {
		e.recordTurn(recordedTurn{
			User:      fmt.Sprintf("问题%d", i),
			Assistant: fmt.Sprintf("回答%d", i),
			Transcript: &TranscriptV1{V: 1, Messages: []TranscriptMessage{
				{Role: openai.ChatMessageRoleTool, Content: fmt.Sprintf("证据%d", i)},
			}},
		})
	}

	assert.Len(t, e.recentTurns, shortTurnSessionPairs,
		"all transcripts that fit the size budget should be retained")

	// Older records are shed when the size bound is reached.
	big := &Engine{}
	big.recordTurn(recordedTurn{User: "旧", Assistant: "旧答",
		Transcript: &TranscriptV1{V: 1, Messages: []TranscriptMessage{
			{Role: openai.ChatMessageRoleTool, Content: strings.Repeat("资", maxRawHistoryRunes)},
		}}})
	big.recordTurn(recordedTurn{User: "新", Assistant: "新答"})
	require.Len(t, big.recentTurns, 1, "an oversized older record must be shed")
	assert.Equal(t, "新", big.recentTurns[0].User, "and it is the OLDER one that goes")

	// Keep the newest record even when it alone exceeds the budget; it is the
	// exchange most likely to be referenced by the next turn.
	lone := &Engine{}
	lone.recordTurn(recordedTurn{User: "大", Assistant: "大答",
		Transcript: &TranscriptV1{V: 1, Messages: []TranscriptMessage{
			{Role: openai.ChatMessageRoleTool, Content: strings.Repeat("资", maxRawHistoryRunes+1)},
		}}})
	assert.Len(t, lone.recentTurns, 1,
		"an over-budget newest record was dropped; the turn just answered has no transcript")
}

func TestRecordedTurnKeyDoesNotConfuseAmbiguousSplits(t *testing.T) {

	require.NotEqual(t, recordedTurnKey("再试", "一次"), recordedTurnKey("再", "试一次"),
		"two different exchanges must not share a key")

	e := &Engine{}
	e.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system"},
		{Role: openai.ChatMessageRoleUser, Content: "再"},
		{Role: openai.ChatMessageRoleAssistant, Content: "试一次"},
	}
	e.recentTurns = []recordedTurn{{
		User: "再试", Assistant: "一次",
		Transcript: &TranscriptV1{V: 1, Messages: []TranscriptMessage{
			{Role: openai.ChatMessageRoleTool, Content: "另一台机器的证据"},
		}},
	}}

	pairs := e.recentConversationPairs()

	require.Len(t, pairs, 1)
	assert.Empty(t, pairs[0].Transcript,
		"a different exchange's tool evidence was attached to this answer")
}

func conversationPairsRunes(pairs []ConversationPair) int {
	total := 0
	for _, pair := range pairs {
		total += conversationPairRenderedRunes(pair)
	}
	return total
}
