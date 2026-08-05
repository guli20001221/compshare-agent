package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	openai "github.com/sashabaranov/go-openai"
)

func TestConversationMemoryCompactorAcceptsOnlySourcedSemanticDelta(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: `{
  "goals":[],
  "constraints":[{"value":"区域保持上海","pair_index":1,"quote":"区域还是上海"}],
  "decisions":[{"value":"采用第二种方案","pair_index":1,"quote":"第二种"}],
  "unresolved_tasks":[]
}`}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "比较一下两个方案"},
		{Role: openai.ChatMessageRoleAssistant, Content: "第一种省钱，第二种更稳定"},
		{Role: openai.ChatMessageRoleUser, Content: "第二种，区域还是上海"},
		{Role: openai.ChatMessageRoleAssistant, Content: "好的，后续按这个配置继续"},
	}

	eng.compactEvictedConversation(context.Background(), messages, time.Unix(3_000, 0))

	state, _, _ := eng.SessionStateSnapshot()
	assert.Contains(t, state.ConversationDigest.Decisions, "采用第二种方案")
	assert.Contains(t, state.ConversationDigest.Constraints, "区域保持上海")
	require.Len(t, state.ConversationDigest.Sources.Decisions, 1)
	assert.Equal(t, "第二种", state.ConversationDigest.Sources.Decisions[0].Quote)
	assert.Empty(t, state.ConversationDigest.Excerpts)
	assert.Equal(t, int64(2), state.ConversationDigest.SummaryFrontier)
	require.Len(t, mock.calls, 1)
}

func TestConversationMemoryCompactorInvalidSourceFallsBackToVerbatimExcerpt(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: `{
  "goals":[],"constraints":[],
  "decisions":[{"value":"擅自猜出的决定","pair_index":0,"quote":"原文中不存在"}],
  "unresolved_tasks":[]
}`}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "同配置再看上海"},
		{Role: openai.ChatMessageRoleAssistant, Content: "我会继续比较"},
	}

	eng.compactEvictedConversation(context.Background(), messages, time.Unix(3_001, 0))

	state, _, _ := eng.SessionStateSnapshot()
	assert.Empty(t, state.ConversationDigest.Decisions)
	require.Len(t, state.ConversationDigest.Excerpts, 1)
	assert.Equal(t, "同配置再看上海", state.ConversationDigest.Excerpts[0].User)
	assert.Equal(t, int64(1), state.ConversationDigest.SummaryFrontier)
}

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

	// Mirror turn entry: refreshConversationDigest still runs, and still merges the
	// task's constraints/decisions into the digest. What changed is that nothing
	// renders them any more.
	eng.refreshConversationDigest(now)
	card := renderAgentContextCard((ContextCompiler{}).CompileForTurn(eng, "现在怎么样", "", now))

	assert.NotContains(t, card, "上次意图",
		"the last-routed-intent line is deliberately gone: nothing refreshes it, so it could only carry a stale pre-P6 value")
	assert.Contains(t, card, "train-box")
	assert.Contains(t, card, "uhost-123")
	assert.Contains(t, card, "把训练机监控看板恢复")
	// NOTE ON WHAT THIS NO LONGER ASSERTS. Task constraints and decisions used to
	// reach the card indirectly: refreshConversationDigest merged them into
	// ConversationDigest.Constraints/Decisions and the card rendered them as
	// 既有约束 / 已作决定. Those blocks are deleted — the canonical transcript
	// replays the turns themselves — so the merge still runs but nothing the
	// model reads consumes it.
	//
	// The consequence worth naming: turns older than the replay window
	// (maxAgentContextPairs) had the digest summary as their only survivor, and
	// now have none. That gap is not introduced here — suppressing the blocks
	// produced it in production on 2026-08-04 — but this deletion makes it
	// permanent, and closing it is what the token-budget compaction step is for.
	assert.NotContains(t, card, "只看最近五分钟")
	assert.NotContains(t, card, "先重启监控 agent")
	assert.Contains(t, card, "该任务已过期", "the task-expired notice must survive in the card")
	assert.Contains(t, card, "必须重新查询")
	assert.NotContains(t, card, "Running",
		"expired observations may retain topic and time, never their old value")
}

func TestTrimHistoryCompaction_OffKeepsCountTrimBehavior(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	// Derived, not hardcoded: a literal 50 was sized against the old ceiling of 40,
	// so raising the ceiling would drop this test into trimHistory's no-op branch —
	// green, and testing nothing.
	pairs := maxHistoryMessages
	eng.messages = makePlainHistory(pairs)
	require.Greater(t, len(eng.messages), 1+maxHistoryMessages, "input must overflow the ceiling")

	eng.trimHistory()

	require.Len(t, eng.messages, 1+maxHistoryMessages)
	assert.Equal(t, openai.ChatMessageRoleSystem, eng.messages[0].Role)
	assert.Equal(t, 1, countSystemMessages(eng.messages), "trim never injects a second system block")
	assert.Equal(t, fmt.Sprintf("a%d", pairs-1), eng.messages[len(eng.messages)-1].Content)
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
	// compaction path actually runs. Derived from maxHistoryMessages: a hardcoded 10
	// pairs overflowed the old ceiling of 40 but not a larger one, which would leave
	// this test asserting compaction behaviour on a history that was never compacted.
	prePairs := maxHistoryMessages
	for i := 0; i < prePairs; i++ {
		eng.messages = append(eng.messages,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: fmt.Sprintf("pre-q%d", i)},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: fmt.Sprintf("pre-a%d", i)},
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
	require.Greater(t, len(eng.messages), 1+maxHistoryMessages)

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
	msgs := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "system prompt"}}
	for i := 0; i < pairs; i++ {
		msgs = append(msgs,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: fmt.Sprintf("q%d", i)},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: fmt.Sprintf("a%d", i)},
		)
	}
	return msgs
}

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
