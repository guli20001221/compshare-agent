package engine

import (
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The history ceiling used to be 40 messages, sized in a comment for "a 32K
// context window". Two things were wrong with that, and only the second one bites.
//
// The unit counts MESSAGES, and e.messages carries every tool response. A fast-path
// turn costs ~2 messages (user + assistant). An agent-loop turn costs ~4-6 (user +
// assistant-with-tool_calls + N tool results + final assistant). So the SAME
// constant buys ~20 fast-path turns but only ~8 agent-loop turns.
//
// That matters because the whole direction of travel is to route every intent
// through the agent loop, to buy back the context the fast path throws away. Doing
// that at 40 would have re-introduced amnesia at turn 8 — inside real session
// lengths (the traffic sample reaches 10 turns and is truncated there, so 10 is a
// floor) — and it would have looked like a model failure, not a config one.
//
// These tests encode the requirement, not the number: the ceiling must clear a
// realistic agent-loop session with room to spare. They fail if someone lowers the
// constant back toward the old value, and they keep failing for the right reason.

// messagesPerAgentLoopTurn is the observed shape of one agent-loop turn:
// user, assistant(tool_calls), tool result, final assistant.
const messagesPerAgentLoopTurn = 4

// longestObservedSessionTurns is a FLOOR, not a maximum: the real-traffic sample
// (443 sessions) is truncated at exactly 10 user turns, so sessions at least this
// long exist and longer ones are invisible to us rather than absent.
const longestObservedSessionTurns = 10

func TestHistoryCeilingClearsARealAgentLoopSession(t *testing.T) {
	needed := longestObservedSessionTurns * messagesPerAgentLoopTurn

	assert.Greater(t, maxHistoryMessages, needed,
		"maxHistoryMessages=%d cannot hold the longest session we have actually observed "+
			"(%d turns x ~%d messages = %d) once that session runs through the agent loop; "+
			"history would be trimmed mid-session and the user would experience amnesia",
		maxHistoryMessages, longestObservedSessionTurns, messagesPerAgentLoopTurn, needed)

	// And it must not merely squeak past: sessions longer than the truncated sample
	// certainly exist. Require 2x headroom over the observed floor.
	assert.GreaterOrEqual(t, maxHistoryMessages, 2*needed,
		"maxHistoryMessages=%d leaves no headroom above the observed session floor (%d); "+
			"the traffic sample is truncated, so longer sessions exist and are simply unseen",
		maxHistoryMessages, needed)
}

// The ceiling also has to stay UNDER the real constraint, which is not the model's
// context window (flash's is enormous) but agent.rate_limit.max_tokens_per_turn.
// A ceiling that overflows it turns a memory fix into rate-limit rejections.
func TestHistoryCeilingStaysUnderThePerTurnTokenCap(t *testing.T) {
	const (
		maxTokensPerTurn      = 200_000 // deploy/conf/config.prod.yaml agent.rate_limit
		approxTokensPerMsg    = 675     // 27K/40, the estimate the old comment itself used
		approxSystemPromptTok = 7_000   // measured: 14,744 bytes of CJK
	)
	worstCase := maxHistoryMessages*approxTokensPerMsg + approxSystemPromptTok

	assert.Less(t, worstCase, maxTokensPerTurn,
		"a full history at maxHistoryMessages=%d is ~%d tokens, over the %d per-turn cap — "+
			"turns would be rejected by the rate limiter instead of remembering more",
		maxHistoryMessages, worstCase, maxTokensPerTurn)
}

// trimHistory must actually preserve a full agent-loop session rather than just
// having a large constant next to it. This drives the real code path.
func TestTrimHistoryKeepsAFullAgentLoopSessionIntact(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "sys"}}

	// Replay a 10-turn agent-loop session, tool messages and all. A turn that makes
	// TWO tool calls (look the instance up, then check its monitor) is ordinary, not
	// a worst case — using the 4-message minimum here would park the test exactly on
	// the old 41-message boundary, where trimHistory returns without trimming, and
	// it would pass at the broken ceiling.
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
