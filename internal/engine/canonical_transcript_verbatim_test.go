package engine

import (
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

// A billing turn is the one shape where the persisted reply and the model's own
// history deliberately DISAGREE, and it is the shape nothing tested against the
// transcript.
//
// The engine delivers a cost breakdown to the user byte-identically and puts an
// amount-free note in e.messages instead (engine.go, verbatimBlockObservation),
// so the Agent "neither re-derives an amount nor claims it cannot look pricing
// up". engine.go states the intent twice: the block "is intentionally NOT
// recorded, so the figures stay out of the model's context".
//
// That intent held only while the process lived. composeWithVerbatimBlocks puts
// the block back into the RETURNED reply, which is what messages.content stores,
// so a restart read the figures straight back into e.messages and replayed them
// — a hot/cold divergence that predates the transcript and that no test named.
//
// Enabling the transcript changes this, in the direction the comments ask for:
// the projected transcript replaces the plain pair, and its tail is the
// amount-free message the hot engine actually held. Both states are pinned here.
// If a future change makes the flag-on path replay figures again, that is a
// regression against a stated intent, and this is what says so.
func TestVerbatimBillingBlockAndTheTranscript(t *testing.T) {
	const (
		question = "这台机器这个月花了多少钱"
		// The figure is the assertion subject: a distinctive string that can only
		// have arrived via the verbatim block.
		amount  = "1234.56"
		tail    = "已确认，明细见上。"
		nextAsk = "那第二台呢"
	)
	block := "费用明细\n计算资源 " + amount + " 元"

	// Compose through the real function rather than hand-writing the stored
	// string: a hand-written one would still pass if composition changed, and the
	// whole point is that the stored reply and e.messages differ.
	composer := &Engine{verbatimBlocksThisTurn: []string{block}}
	persistedReply := composer.composeWithVerbatimBlocks(tail)
	require.Contains(t, persistedReply, amount,
		"fixture is wrong: the persisted reply is what carries the figures")

	// What the hot engine holds after the round: the block never enters
	// e.messages, the amount-free observation stands in for the tool result.
	hotTurn := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: question},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{
			toolCall("c1", "DescribeCompShareInstance", `{"UHostId":"uhost-a"}`)}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "c1", Content: verbatimBlockObservation},
		{Role: openai.ChatMessageRoleAssistant, Content: tail},
	}
	require.NotContains(t, renderTestMessages(hotTurn), amount,
		"fixture is wrong: e.messages must not hold the figures — that is the property under test")

	t.Run("flag on: the transcript replays the amount-free record", func(t *testing.T) {
		prev := canonicalTranscriptEnabled
		SetCanonicalTranscriptEnabled(true)
		defer SetCanonicalTranscriptEnabled(prev)

		_, metadata, stats := runHotTurn(hotTurn)
		require.True(t, stats.Attempted, "a tool-bearing billing turn must be captured")
		require.NotEmpty(t, metadata)

		cold := rebuildCold(question, persistedReply, metadata)
		assembled := assembleNextTurn(cold, nextAsk)
		requireTranscriptWasReplayed(t, assembled)

		replayed := renderReplayedRegion(t, assembled)
		require.NotContains(t, replayed, amount,
			"the transcript replayed the cost figures the engine deliberately withholds")
		require.Contains(t, replayed, verbatimBlockObservation,
			"the model must still learn a breakdown was delivered, or it re-derives one")
	})

	t.Run("flag off: the stored reply replays the figures", func(t *testing.T) {
		prev := canonicalTranscriptEnabled
		SetCanonicalTranscriptEnabled(false)
		defer SetCanonicalTranscriptEnabled(prev)

		cold := rebuildCold(question, persistedReply, nil)
		assembled := assembleNextTurn(cold, nextAsk)

		replayed := renderReplayedRegion(t, assembled)
		// Not an endorsement. This documents the state a rollback returns to, so
		// the flag-on assertion above cannot be mistaken for "nothing changed".
		require.Contains(t, replayed, amount,
			"pre-existing behaviour changed: with the transcript off, a restart "+
				"replays messages.content, which carries the composed block")
	})
}

// The content matcher is what decides whether the transcript attaches at all,
// and a billing turn is where the two sides could most plausibly disagree: the
// hot record is written from e.messages (amount-free) while the cold record is
// written from the persisted row (composed). If those two were ever compared to
// each other, the attach would silently stop happening on exactly the turns that
// motivated the intent above — and no parity test would see it, because a parity
// test compares hot replay to cold replay, and neither would carry a transcript.
func TestVerbatimBillingTurnStillMatchesItsRecord(t *testing.T) {
	enableCanonicalTranscriptForTest(t)

	const question = "这个月账单"
	tail := "已确认。"
	composer := &Engine{verbatimBlocksThisTurn: []string{"费用明细\n合计 99.90 元"}}
	persistedReply := composer.composeWithVerbatimBlocks(tail)

	hot, metadata, _ := runHotTurn([]openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: question},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{
			toolCall("c1", "DescribeCompShareInstance", `{}`)}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "c1", Content: verbatimBlockObservation},
		{Role: openai.ChatMessageRoleAssistant, Content: tail},
	})

	// Hot: both sides read e.messages, so the record names the amount-free tail.
	require.Len(t, hot.recentTurns, 1)
	require.Equal(t, tail, hot.recentTurns[0].Assistant,
		"the hot record must name what e.messages holds, not the composed reply")

	hotDelta := replayDelta(t, func() { hot.recentCompleteConversationPairs(maxAgentContextPairs) })
	require.Zero(t, hotDelta.MatchMissed, "hot pair and hot record disagreed on a billing turn")
	require.Equal(t, int64(1), hotDelta.TranscriptsAttached)

	// Cold: both sides read the persisted row, so the record names the composed
	// reply. Different string, same agreement — which is why the attach survives.
	cold := rebuildCold(question, persistedReply, metadata)
	require.Len(t, cold.recentTurns, 1)
	require.Equal(t, persistedReply, cold.recentTurns[0].Assistant)
	require.True(t, strings.Contains(persistedReply, tail) && persistedReply != tail,
		"fixture is wrong: hot and cold must be recording genuinely different strings here")

	coldDelta := replayDelta(t, func() { cold.recentCompleteConversationPairs(maxAgentContextPairs) })
	require.Zero(t, coldDelta.MatchMissed, "cold pair and cold record disagreed on a billing turn")
	require.Equal(t, int64(1), coldDelta.TranscriptsAttached)
}
