package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	openai "github.com/sashabaranov/go-openai"
)

// The single context card must carry every structured signal the retired
// buildReActHistorySummary block surfaced: task-level constraints/decisions, the
// task-expired notice, the selected instance, and the expired-observation
// "re-query" caution — all must still reach the model through
// renderAgentContextCard, and an expired fact's stale value must never leak.
//
// DELIBERATELY NARROWED: the block's sixth signal, "上次意图", is no longer
// promised. SessionState.LastIntent lost its last Go writer when P6 deleted the
// route stack, so the only values that field could still hold came from rows a
// pre-P6 binary persisted — a classification nothing refreshes, re-persisted
// verbatim every turn. That is stale assertion, not memory, so the field and its
// card line were removed rather than re-wired. This test is the record of that
// narrowing being a decision; the remaining assertions are unchanged.
func TestContextCard_IsStrictSupersetOfRetiredHistorySummary(t *testing.T) {
	now := time.Unix(1_000, 0)
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{
		SchemaVersion:        SessionStateSchemaCurrent,
		SelectedInstanceID:   "uhost-123",
		SelectedInstanceName: "train-box",
		TaskSnapshot: TaskSnapshot{
			Goal:        "把训练机监控看板恢复",
			Constraints: []string{"只看最近五分钟"},
			Decisions:   []string{"先重启监控 agent"},
			Status:      TaskSnapshotStatusExpired,
			Freshness:   ContinuityFreshnessExpired,
		},
		RecentFacts: []ToolFact{{
			Kind:           FactKindInstanceState,
			SubjectID:      "uhost-expired",
			Payload:        map[string]any{"state": "Running"},
			ProducedAtUnix: now.Unix() - 90,
			TTLSeconds:     30,
		}},
	}, 1)

	card := renderAgentContextCard((ContextCompiler{}).CompileForTurn(eng, "现在怎么样", "", now))

	assert.NotContains(t, card, "上次意图",
		"the last-routed-intent line is deliberately gone: nothing refreshes it, so it could only carry a stale pre-P6 value")
	assert.Contains(t, card, "train-box")
	assert.Contains(t, card, "uhost-123")
	assert.Contains(t, card, "把训练机监控看板恢复")
	// NOTE ON WHAT THIS NO LONGER ASSERTS. Task constraints and decisions used to
	// reach the card indirectly: refreshConversationDigest merged them into
	// ConversationDigest.Constraints/Decisions and the card rendered them as
	// 既有约束 / 已作决定. Both the blocks and the merge are now deleted; the
	// canonical transcript replays the turns that produced them.
	//
	// The cost this was once thought to carry — that turns older than the replay
	// window had the digest summary as their only survivor — did not materialise
	// on the shipped path and should not be cited as one. History compaction is
	// what fed the digest, and it cannot fire inside a session:
	// stripHistoricalToolTranscript leaves 2 messages per turn, so its trigger
	// needs 61 turns while agent.http.max_session_turns caps the compatibility
	// path at 20. Measured against the replay database, 0 of 127 sessions held a
	// single digest excerpt.
	assert.NotContains(t, card, "只看最近五分钟")
	assert.NotContains(t, card, "先重启监控 agent")
	assert.Contains(t, card, "该任务已过期", "the task-expired notice must survive in the card")
	assert.Contains(t, card, "必须重新查询")
	assert.NotContains(t, card, "Running",
		"expired observations may retain topic and time, never their old value")
}

func TestTrimHistoryCompaction_OffKeepsPlainTrimBehavior(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	const padRunes = 400
	pairs := overflowingPairs(padRunes)
	eng.messages = makePaddedHistory(pairs, padRunes)
	require.Greater(t, assembledRequestRunes(eng.messages[1:]), maxRawHistoryRunes,
		"input must overflow the ceiling")

	eng.trimHistory()

	require.LessOrEqual(t, assembledRequestRunes(eng.messages[1:]), maxRawHistoryRunes)
	require.Less(t, len(eng.messages), 1+2*pairs, "the trim must actually have fired")
	assert.Equal(t, openai.ChatMessageRoleSystem, eng.messages[0].Role)
	assert.Equal(t, 1, countSystemMessages(eng.messages), "trim never injects a second system block")
	assert.True(t, strings.HasPrefix(eng.messages[len(eng.messages)-1].Content, fmt.Sprintf("a%d", pairs-1)))
}

func TestTrimHistoryCompaction_TrimsToolPairsAndShrinksOldToolResultsWithoutSummaryBlock(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetReactHistoryCompactionEnabled(true)
	eng.SetSessionState(SessionState{
		SelectedInstanceID:   "uhost-selected",
		SelectedInstanceName: "selected-box",
	}, 1)
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "system"}}
	// Leading padding whose only job is to push the history over the ceiling so the
	// compaction path actually runs. Derived from the ceiling: a hardcoded 10 pairs
	// overflowed the old count of 40 but not a larger one, which would leave this
	// test asserting compaction behaviour on a history that was never compacted.
	const prePad = 400
	prePairs := overflowingPairs(prePad)
	for i := 0; i < prePairs; i++ {
		eng.messages = append(eng.messages,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser,
				Content: fmt.Sprintf("pre-q%d", i) + strings.Repeat("问", prePad)},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant,
				Content: fmt.Sprintf("pre-a%d", i) + strings.Repeat("答", prePad)},
		)
	}
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("tc%d", i)
		eng.messages = append(eng.messages,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: fmt.Sprintf("tool-q%d", i)},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{toolCall(id, "DescribeCompShareInstance", `{}`)}},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: id, Content: fmt.Sprintf(`{"RetCode":0,"Action":"DescribeCompShareInstanceResponse","Huge":"%s"}`, strings.Repeat(fmt.Sprintf("huge-%d", i), 60))},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: fmt.Sprintf("tool-a%d", i)},
		)
	}
	eng.messages = append(eng.messages,
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "tail-q"},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "tail-a"},
	)
	require.Greater(t, assembledRequestRunes(eng.messages[1:]), maxRawHistoryRunes,
		"input must overflow the ceiling or the compaction path is a no-op")

	eng.trimHistoryByCompaction(time.Unix(2_000, 0))

	require.Greater(t, len(eng.messages), 2)
	assert.Equal(t, openai.ChatMessageRoleSystem, eng.messages[0].Role)
	assert.Equal(t, 1, countSystemMessages(eng.messages),
		"compaction trims and shrinks but never injects a second system/summary block")
	assert.NotEqual(t, openai.ChatMessageRoleSystem, eng.messages[1].Role,
		"the first kept message after the base prompt is real history, not a summary")
	assertToolCallPairsValid(t, eng.messages)

	compacted := 0
	preservedHuge := 0
	for _, msg := range eng.messages {
		if msg.Role != openai.ChatMessageRoleTool {
			continue
		}
		if strings.HasPrefix(msg.Content, reactHistoryCompactedToolPrefix) {
			compacted++
		}
		if strings.Contains(msg.Content, "huge-") {
			preservedHuge++
		}
	}
	assert.Equal(t, 3, compacted, "six retrievable results should keep only the three most recent full payloads")
	assert.Equal(t, 3, preservedHuge)
	assert.Equal(t, "tail-a", eng.messages[len(eng.messages)-1].Content)

	eng.trimHistoryByCompaction(time.Unix(2_001, 0))
	assert.Equal(t, 1, countSystemMessages(eng.messages), "repeated compaction must not add a system block")
	assertToolCallPairsValid(t, eng.messages)
}

func TestCompactOldRetrievableToolResults_KeepsErrorsAndWorkflowResults(t *testing.T) {
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{toolCall("ok-old", "DescribeCompShareImages", `{}`)}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "ok-old", Content: `{"RetCode":0,"ImageSet":[{"id":"old"}]}`},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{toolCall("err", "DescribeCompShareImages", `{}`)}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "err", Content: `{"RetCode":100,"Message":"api failed"}`},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{toolCall("wf", "StopInstanceWorkflow", `{}`)}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "wf", Content: `{"RetCode":0,"Result":"confirmed"}`},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{toolCall("ok-new", "DescribeCompShareImages", `{}`)}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "ok-new", Content: `{"RetCode":0,"ImageSet":[{"id":"new"}]}`},
	}

	compactOldRetrievableToolResults(msgs, 1)

	assert.Contains(t, msgs[1].Content, reactHistoryCompactedToolPrefix)
	assert.Contains(t, msgs[3].Content, "api failed")
	assert.Contains(t, msgs[5].Content, "confirmed")
	assert.Contains(t, msgs[7].Content, `"new"`)
}

func makePlainHistory(pairs int) []openai.ChatCompletionMessage {
	return makePaddedHistory(pairs, 0)
}

// makePaddedHistory pads each message so a fixture can overflow a SIZE ceiling
// without needing tens of thousands of messages to do it. The tag stays at the
// front of the content so assertions can still identify a message by prefix.
func makePaddedHistory(pairs, padRunes int) []openai.ChatCompletionMessage {
	msgs := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "system prompt"}}
	for i := 0; i < pairs; i++ {
		msgs = append(msgs,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser,
				Content: fmt.Sprintf("q%d", i) + strings.Repeat("问", padRunes)},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant,
				Content: fmt.Sprintf("a%d", i) + strings.Repeat("答", padRunes)},
		)
	}
	return msgs
}

// overflowingPairs is how many padded pairs it takes to exceed maxRawHistoryRunes
// — derived so that changing the ceiling re-sizes every fixture instead of
// dropping it silently into a no-op branch. A literal 50 was once sized against a
// ceiling of 40 and did exactly that when the ceiling moved.
func overflowingPairs(padRunes int) int { return maxRawHistoryRunes/(2*padRunes) + 2 }

func countSystemMessages(messages []openai.ChatCompletionMessage) int {
	count := 0
	for _, msg := range messages {
		if msg.Role == openai.ChatMessageRoleSystem {
			count++
		}
	}
	return count
}

func assertToolCallPairsValid(t *testing.T, messages []openai.ChatCompletionMessage) {
	t.Helper()
	toolResponses := map[string]bool{}
	for _, msg := range messages {
		if msg.Role == openai.ChatMessageRoleTool {
			toolResponses[msg.ToolCallID] = true
		}
	}
	for _, msg := range messages {
		if msg.Role != openai.ChatMessageRoleAssistant {
			continue
		}
		for _, call := range msg.ToolCalls {
			assert.Truef(t, toolResponses[call.ID], "tool_call %s must have a matching tool response", call.ID)
		}
	}
}
