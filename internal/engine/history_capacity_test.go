package engine

import (
	"testing"

	"github.com/compshare-agent/internal/config"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The raw-history ceiling has been wrong twice in the same way. It was 40
// MESSAGES, sized in a comment for "a 32K context window" — a model we no longer
// run — which bought ~20 fast-path turns but only ~8 agent-loop turns, because
// e.messages carries every tool response. Raising it to 120 moved the number out
// of the way of real traffic without fixing the unit, and its own comment said so.
//
// It is now maxRawHistoryRunes, a size. These tests encode the requirement rather
// than the number, and there are two distinct requirements: it must clear a real
// session, and it must never be the thing that decides what the model remembers.

// longestObservedSessionTurns is a FLOOR, not a maximum: the real-traffic sample
// (443 sessions) is truncated at exactly 10 user turns, so sessions at least this
// long exist and longer ones are invisible to us rather than absent.
const longestObservedSessionTurns = 10

// largestObservedSessionHistoryRunes is the biggest whole-session plain-text
// history in the three 2026-07 production exports. It is the right unit to
// compare against: what survives into e.messages is exactly plain user/assistant
// text, because stripHistoricalToolTranscript deletes the tool payloads before
// the ceiling is ever applied.
const largestObservedSessionHistoryRunes = 11271

func TestHistoryCeilingClearsARealSession(t *testing.T) {
	assert.Greater(t, maxRawHistoryRunes, largestObservedSessionHistoryRunes,
		"maxRawHistoryRunes=%d cannot hold the largest session history we have actually "+
			"observed (%d runes); it would be trimmed mid-session and the user would "+
			"experience amnesia",
		maxRawHistoryRunes, largestObservedSessionHistoryRunes)

	// And it must not merely squeak past: the export is a sample, so larger
	// sessions exist and are simply unseen.
	assert.GreaterOrEqual(t, maxRawHistoryRunes, 2*largestObservedSessionHistoryRunes,
		"maxRawHistoryRunes=%d leaves no headroom above the observed maximum (%d)",
		maxRawHistoryRunes, largestObservedSessionHistoryRunes)
}

// The requirement that replaced the count caps, stated exactly. Both lists the
// replay draws from are budgeted at maxRawHistoryRunes; if that were below
// maxReplayedHistoryRunes, a source would run out before the budget did and the
// model's memory would be decided by a number that does not claim to decide it —
// which is the failure the deleted counts were causing.
func TestNoSourceListShadowsTheReplayBudget(t *testing.T) {
	assert.GreaterOrEqual(t, maxRawHistoryRunes, maxReplayedHistoryRunes,
		"maxRawHistoryRunes=%d is below maxReplayedHistoryRunes=%d: e.messages and "+
			"e.recentTurns would both run dry before the replay budget did, and the "+
			"model's cross-turn memory would silently be %d runes, not %d",
		maxRawHistoryRunes, maxReplayedHistoryRunes, maxRawHistoryRunes, maxReplayedHistoryRunes)
}

// The ceiling also has to stay UNDER the real constraint, which is not the
// model's context window but agent.rate_limit.max_tokens_per_turn. A ceiling that
// overflows it turns a memory fix into rate-limit rejections.
func TestHistoryCeilingStaysUnderThePerTurnTokenCap(t *testing.T) {
	// Runes and tokens are interchangeable here: the probe behind
	// measuredContextWindowFloorTokens billed 130,000 CJK runes as 130,006 prompt
	// tokens. The old form of this test used 675 tokens per MESSAGE, an estimate
	// the pre-2026 comment had made up for a 40-message ceiling.
	const approxSystemPromptTok = 2000 // measured 1,932 via prompt.BuildSystemWithOptions
	worstCase := maxRawHistoryRunes + approxSystemPromptTok

	assert.Less(t, worstCase, config.ShippedMaxTokensPerTurn,
		"a full history at maxRawHistoryRunes=%d is ~%d tokens, over the %d per-turn cap — "+
			"turns would be rejected by the rate limiter instead of remembering more",
		maxRawHistoryRunes, worstCase, config.ShippedMaxTokensPerTurn)
}

// trimHistory must actually preserve a full agent-loop session rather than just
// having a large constant next to it. This drives the real code path.
func TestTrimHistoryKeepsAFullAgentLoopSessionIntact(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "sys"}}

	// Replay a 10-turn agent-loop session, tool messages and all. A turn that makes
	// TWO tool calls (look the instance up, then check its monitor) is ordinary, not
	// a worst case.
	for turn := 1; turn <= longestObservedSessionTurns; turn++ {
		eng.messages = append(eng.messages,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: firstTurnMarker(turn)},
			openai.ChatCompletionMessage{
				Role: openai.ChatMessageRoleAssistant,
				ToolCalls: []openai.ToolCall{
					toolCall("tc1", "DescribeCompShareInstance", `{}`),
					toolCall("tc2", "DescribeCompShareMonitor", `{}`),
				},
			},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "tc1", Content: "{}"},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "tc2", Content: "{}"},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "reply"},
		)
		eng.trimHistory()
	}

	// The user's FIRST message is what an elliptical follow-up ("那台呢") depends on.
	// If trimming ate it, the agent has forgotten what the conversation is about.
	require.NotEmpty(t, eng.messages)
	var found bool
	for _, m := range eng.messages {
		if m.Content == firstTurnMarker(1) {
			found = true
			break
		}
	}
	assert.True(t, found,
		"the opening user message was trimmed out of a %d-turn agent-loop session; "+
			"an elliptical follow-up can no longer resolve what the user is talking about",
		longestObservedSessionTurns)
}

func firstTurnMarker(turn int) string {
	return "user-turn-" + string(rune('0'+turn%10)) + "-marker"
}
