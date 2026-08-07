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

// Three fixed COUNT ceilings used to decide how much a turn could carry, beside
// the two size budgets that also decided it:
//
//	maxAgentContextPairs        = config.MaxReplayedExchanges = 20   replayed exchanges
//	maxAssembledRequestMessages = 100                                messages in one request
//	maxHistoryMessages          = 120                                messages in e.messages
//
// All three are deleted. The tests below are stated as the behaviour that a
// reintroduced count would break, not as assertions about the constants — a
// constant can be renamed, and the assertion goes with it.
//
// The direction that matters is SMALL exchanges. A count and a size agree closely
// at one exchange size and diverge everywhere else, and it is at the small end
// that a count is the binding one: 60 short turns cost a twentieth of the replay
// budget and every one of them used to be discarded at 20.

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

// The replay window is a size. Real traffic is full of turns like 「好贵」 and
// 「ComfyUI 打不开」, so a session can run well past 20 exchanges while costing a
// fraction of the budget — and every exchange past the twentieth used to be
// dropped for a reason that had nothing to do with what a request could hold.
func TestReplayWindowKeepsMoreThanTheDeletedCountCeiling(t *testing.T) {
	e := shortTurnEngine(t)

	pairs := e.recentCompleteConversationPairs()

	require.Less(t, conversationPairsRunes(pairs), maxReplayedHistoryRunes,
		"premise: %d short exchanges must genuinely fit the budget, or this test is "+
			"measuring the budget rather than the deleted count", shortTurnSessionPairs)
	assert.Len(t, pairs, shortTurnSessionPairs,
		"only %d of %d exchanges were replayed while the size budget had room for all of "+
			"them; a count ceiling is deciding the model's memory again", len(pairs), shortTurnSessionPairs)
	assert.Equal(t, "问题0", pairs[0].User, "including the session's opening turn")
}

// The same session, through the whole assembler. This is the one that would catch
// a count reintroduced at any of the three places rather than only at the replay
// window: 121 messages reach a request that used to be capped at 100.
func TestAssembledRequestHasNoMessageCountCeiling(t *testing.T) {
	e := shortTurnEngine(t)
	e.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(
		e, "问题0 说的是哪台", "t", time.Unix(1_800_000_000, 0))
	e.turnContextViewReady = true

	out := e.buildMessagesForLLM(productionToolWindow())

	require.Greater(t, len(out), 100,
		"premise: the fixture must exceed the deleted 100-message cap, or nothing is proven")
	assert.LessOrEqual(t, assembledRequestRunes(out)+toolWindowRunes(productionToolWindow()),
		maxAssembledRequestRunes, "and it must still respect the one bound that remains")
	assert.Contains(t, renderTestMessages(out), "问题0",
		"the oldest exchange survived the size budget but not the assembler")
}

// The recorded-transcript window is the third source, and it is the one that can
// fail silently: losing a record does not lose the exchange, it downgrades a
// replayed exchange from "with its tool evidence" to "plain text", which reads as
// the model simply not using the evidence.
func TestRecordedTurnWindowIsSizedNotCounted(t *testing.T) {
	withCanonicalTranscript(t, true)

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
		"only %d of %d transcripts were retained; the exchanges would replay as plain "+
			"text with their tool evidence silently missing", len(e.recentTurns), shortTurnSessionPairs)

	// And the size bound it was replaced by does still bite, in the other
	// direction — otherwise this window would be unbounded rather than re-based.
	big := &Engine{}
	big.recordTurn(recordedTurn{User: "旧", Assistant: "旧答",
		Transcript: &TranscriptV1{V: 1, Messages: []TranscriptMessage{
			{Role: openai.ChatMessageRoleTool, Content: strings.Repeat("资", maxRawHistoryRunes)},
		}}})
	big.recordTurn(recordedTurn{User: "新", Assistant: "新答"})
	require.Len(t, big.recentTurns, 1, "an oversized older record must be shed")
	assert.Equal(t, "新", big.recentTurns[0].User, "and it is the OLDER one that goes")

	// The newest record is kept even when it ALONE exceeds the budget. It is the
	// turn just answered, so the next turn's 「刚才那台」 resolves against it; a
	// budget that discarded it would delete the evidence for the very exchange most
	// likely to be referred to. Only this case separates "keep the newest
	// unconditionally" from "keep whatever fits" — with two records, the newest
	// survives either way, which is why asserting on the pair above is not enough.
	lone := &Engine{}
	lone.recordTurn(recordedTurn{User: "大", Assistant: "大答",
		Transcript: &TranscriptV1{V: 1, Messages: []TranscriptMessage{
			{Role: openai.ChatMessageRoleTool, Content: strings.Repeat("资", maxRawHistoryRunes+1)},
		}}})
	assert.Len(t, lone.recentTurns, 1,
		"an over-budget newest record was dropped; the turn just answered has no transcript")
}

// The index that made transcript matching linear identifies an exchange by
// concatenating its two sides, so the split between them has to be recoverable.
// Without the length prefix, the record ("再试", "一次") and the exchange ("再",
// "试一次") produce the same key — and one turn's tool evidence attaches to a
// different turn's answer. That is the misattribution
// TestAttachRecordedTranscripts_DoesNotReuseOneRecordForRepeatedExchanges exists
// to prevent, reintroduced by the index built to preserve it.
func TestRecordedTurnKeyDoesNotConfuseAmbiguousSplits(t *testing.T) {
	withCanonicalTranscript(t, true)

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

	pairs := e.recentCompleteConversationPairs()

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
