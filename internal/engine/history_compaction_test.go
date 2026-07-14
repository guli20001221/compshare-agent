package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/intent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	openai "github.com/sashabaranov/go-openai"
)

func TestBuildReActHistorySummary_KeepsStructuredSignalsNotFactPayload(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	now := time.Unix(1_000, 0)
	eng.SetSessionState(SessionState{
		SelectedInstanceID:   "uhost-123",
		SelectedInstanceName: "train-box",
		LastIntent:           string(intent.IntentMonitorQuery),
		RecentFacts: []ToolFact{{
			Kind:           FactKindMonitorSample,
			SubjectID:      "uhost-123",
			Payload:        map[string]any{"cpu_usage": float64(88), "gpu_usage": float64(77)},
			ProducedAtUnix: now.Unix() - 5,
			TTLSeconds:     30,
		}, {
			Kind:           FactKindInstanceState,
			SubjectID:      "uhost-expired",
			Payload:        map[string]any{"state": "Running"},
			ProducedAtUnix: now.Unix() - 90,
			TTLSeconds:     30,
		}},
	}, 1)
	eng.lastPlannerActionThisTurn = intent.LifecycleActionStart

	got := eng.buildReActHistorySummary(now)

	assert.Contains(t, got, reactHistorySummaryPrefix)
	assert.Contains(t, got, "train-box")
	assert.Contains(t, got, "uhost-123")
	assert.Contains(t, got, string(intent.IntentMonitorQuery))
	assert.Contains(t, got, string(intent.LifecycleActionStart))
	assert.Contains(t, got, "近期事实引用：uhost-123 monitor_sample")
	assert.NotContains(t, got, "88")
	assert.NotContains(t, got, "gpu_usage")
	assert.Contains(t, got, "历史事实主题：uhost-expired instance_state")
	assert.NotContains(t, got, "Running",
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
	assert.False(t, hasReactHistorySummary(eng.messages))
	assert.Equal(t, fmt.Sprintf("a%d", pairs-1), eng.messages[len(eng.messages)-1].Content)
}

func TestTrimHistoryCompaction_KeepsSummaryToolPairsAndShrinksOldToolResults(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetReactHistoryCompactionEnabled(true)
	eng.SetSessionState(SessionState{
		SelectedInstanceID:   "uhost-selected",
		SelectedInstanceName: "selected-box",
		LastIntent:           string(intent.IntentResourceInfo),
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
	assert.Equal(t, openai.ChatMessageRoleSystem, eng.messages[1].Role)
	assert.Contains(t, eng.messages[1].Content, reactHistorySummaryPrefix)
	assert.Contains(t, eng.messages[1].Content, "selected-box")
	assert.Equal(t, 1, countHistorySummaryMessages(eng.messages))
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
	assert.Equal(t, 1, countHistorySummaryMessages(eng.messages), "repeated compaction must not duplicate the summary")
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

func countHistorySummaryMessages(messages []openai.ChatCompletionMessage) int {
	count := 0
	for _, msg := range messages {
		if msg.Role == openai.ChatMessageRoleSystem && strings.HasPrefix(msg.Content, reactHistorySummaryPrefix) {
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
