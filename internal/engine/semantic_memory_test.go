package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	openai "github.com/sashabaranov/go-openai"
)

func TestSemanticMemory_V7RoundTripAndAdvisoriesStayEphemeral(t *testing.T) {
	state := SessionState{
		SchemaVersion: SessionStateSchemaV7,
		TaskSnapshot: TaskSnapshot{
			Goal:          "给训练机扩容",
			Workflow:      "ResizeDiskWorkflow",
			Stage:         "missing_slots",
			Constraints:   []string{"disk=data", "instance=uhost-a"},
			Decisions:     []string{"target_size_gb=200"},
			MissingSlots:  []string{"disk_id"},
			Status:        TaskSnapshotStatusActive,
			Freshness:     ContinuityFreshnessFresh,
			UpdatedAtUnix: 1_800_000_000,
		},
		ConversationDigest: ConversationDigest{
			Narrative:       "目标：给训练机扩容。未完成：需要选择数据盘",
			Goals:           []string{"给训练机扩容"},
			UnresolvedTasks: []string{"需要选择数据盘"},
			Sources: MemoryDelta{Goals: []SourcedMemory{{
				Value: "给训练机扩容", PairIndex: 0, Quote: "给训练机扩容",
			}}},
			Excerpts:        []ConversationExcerpt{{User: "继续", Assistant: "请先选择数据盘"}},
			SummaryFrontier: 12,
			UpdatedAtUnix:   1_800_000_000,
		},
	}
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(state, 7)
	eng.SetContinuityAdvisories(ContinuityAdvisories{
		ReadOnly: true,
		Notices:  []string{"turn_status=failed_retryable"},
	})

	snapshot, _, hydrated := eng.SessionStateSnapshot()
	require.True(t, hydrated)
	raw, err := json.Marshal(snapshot)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "failed_retryable",
		"ephemeral turn advisories must never become a second persisted truth")
	assert.NotContains(t, string(raw), "continuity_advisories")
	assert.NotContains(t, string(raw), "tool_calls")
	assert.NotContains(t, string(raw), "tool_result")

	envelopeRaw, err := json.Marshal(PersistedContext{AgentSessionState: snapshot})
	require.NoError(t, err)
	roundTrip, err := ParsePersistedContext(envelopeRaw)
	require.NoError(t, err)
	assert.Equal(t, state.TaskSnapshot, roundTrip.AgentSessionState.TaskSnapshot)
	assert.Equal(t, state.ConversationDigest, roundTrip.AgentSessionState.ConversationDigest)
}

func TestChat_ExpiredFrameKeepsSemanticsButCannotResume(t *testing.T) {
	now := time.Now()
	eng := NewWithDeps(
		&mockLLM{responses: []llm.ChatResponse{{Content: "请重新确认后继续"}}},
		&mockExecutor{},
		nil,
	)
	eng.InitWithContext("")
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaV4,
		ContextFrame: ContextFrame{
			Version:         1,
			Kind:            ContextFrameKindWorkflowTask,
			Status:          ContextFrameStatusFailedRecoverable,
			Intent:          "operation_lifecycle",
			Workflow:        "ResizeDiskWorkflow",
			OriginalUserMsg: "把训练机的数据盘扩到 200G",
			Slots:           map[string]string{"instance_id": "uhost-a", "target_size_gb": "200"},
			SlotSources:     map[string]string{"instance_id": SelectedInstanceSourceUser},
			MissingSlots:    []string{"disk_id"},
			Stage:           "missing_slots",
			FailureReason:   "需要选择数据盘",
			ProducedAtUnix:  now.Add(-10 * time.Minute).Unix(),
			TTLSeconds:      ContextFrameTTLSeconds,
		},
	}, 3)

	_, err := eng.Chat(context.Background(), "继续", noopStep)
	require.NoError(t, err)
	state, _, _ := eng.SessionStateSnapshot()

	assert.Equal(t, ContextFrameKindWorkflowTask, state.ContextFrame.Kind,
		"expiry must not erase the task identity")
	assert.Equal(t, ContinuityFreshnessExpired, state.ContextFrame.Freshness)
	assert.Empty(t, state.ContextFrame.SlotSources,
		"expired slots must lose write-authorizing provenance")
	assert.False(t, contextFrameSlotSourceTrusted(state.ContextFrame, "instance_id"))
	assert.Equal(t, "把训练机的数据盘扩到 200G", state.TaskSnapshot.Goal)
	assert.Equal(t, "missing_slots", state.TaskSnapshot.Stage)
	assert.Equal(t, []string{"disk_id"}, state.TaskSnapshot.MissingSlots)
	assert.Equal(t, TaskSnapshotStatusExpired, state.TaskSnapshot.Status)
	assert.Contains(t, state.ConversationDigest.Narrative, "把训练机的数据盘扩到 200G")
	assert.Contains(t, eng.messages[0].Content, "不得直接继续执行")
	_, active := eng.activeContextFrame(time.Now())
	assert.False(t, active, "semantic retention must never reactivate an expired workflow")
}

func TestChat_ExpiredToolFactKeepsTopicAndTimeButDropsValue(t *testing.T) {
	now := time.Now()
	eng := NewWithDeps(
		&mockLLM{responses: []llm.ChatResponse{{Content: "我会重新查询"}}},
		&mockExecutor{},
		nil,
	)
	eng.InitWithContext("")
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaV4,
		RecentFacts: []ToolFact{{
			Kind:           FactKindMonitorSample,
			SubjectID:      "uhost-a",
			Payload:        map[string]any{"cpu_usage": "SECRET_OLD_99"},
			ProducedAtTurn: 2,
			ProducedAtUnix: now.Add(-10 * time.Minute).Unix(),
			TTLSeconds:     factTTLSecondsMonitorSample,
		}},
	}, 4)

	_, err := eng.Chat(context.Background(), "那现在呢", noopStep)
	require.NoError(t, err)
	state, _, _ := eng.SessionStateSnapshot()
	require.Len(t, state.RecentFacts, 1)
	fact := state.RecentFacts[0]
	assert.Equal(t, "uhost-a", fact.SubjectID)
	assert.Equal(t, FactKindMonitorSample, fact.Kind)
	assert.Equal(t, now.Add(-10*time.Minute).Unix(), fact.ProducedAtUnix)
	assert.Nil(t, fact.Payload)
	assert.Equal(t, ContinuityFreshnessExpired, fact.Freshness)
	assert.True(t, fact.RefreshRequired)
	assert.Contains(t, eng.messages[0].Content, "当前值必须重新查询")
	assert.NotContains(t, eng.messages[0].Content, "SECRET_OLD_99")
}

func TestTrimHistory_RemovesRawToolTranscriptAndKeepsSemanticSummary(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaV5,
		TaskSnapshot: TaskSnapshot{
			Goal:         "把训练机的数据盘扩到 200G",
			Constraints:  []string{"instance=uhost-a"},
			Decisions:    []string{"target_size_gb=200"},
			MissingSlots: []string{"disk_id"},
			Status:       TaskSnapshotStatusActive,
			Freshness:    ContinuityFreshnessFresh,
		},
	}, 1)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system"},
		{Role: openai.ChatMessageRoleUser, Content: "查一下实例"},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{toolCall("tc-raw", "DescribeCompShareInstance", "{}")}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "tc-raw", Content: "{\"SecretRawToolJSON\":\"MUST_NOT_SURVIVE\"}"},
		{Role: openai.ChatMessageRoleAssistant, Content: "查到了，接下来要扩盘。"},
	}

	eng.trimHistory()

	for _, msg := range eng.messages {
		assert.NotEqual(t, openai.ChatMessageRoleTool, msg.Role)
		assert.Empty(t, msg.ToolCalls)
		assert.NotContains(t, msg.Content, "MUST_NOT_SURVIVE")
	}
	assert.Equal(t, "查到了，接下来要扩盘。", eng.messages[len(eng.messages)-1].Content)

	summary := eng.buildReActHistorySummary(time.Now())
	assert.Contains(t, summary, "把训练机的数据盘扩到 200G")
	assert.Contains(t, summary, "instance=uhost-a")
	assert.Contains(t, summary, "target_size_gb=200")
	assert.Contains(t, summary, "disk_id")
	assert.False(t, strings.Contains(summary, "MUST_NOT_SURVIVE"))
}

func TestTrimHistory_PreservesDiscardedTurnsWithoutGuessingTheirMeaning(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaV5}, 1)
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "system"}}
	for i := 0; i < maxHistoryMessages; i++ {
		user := "普通问题"
		if i < maxHistoryMessages/2 {
			user = "第二种，区域保持不变"
		}
		eng.messages = append(eng.messages,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: user},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "已记录"},
		)
	}
	require.Greater(t, len(eng.messages), 1+maxHistoryMessages)

	eng.trimHistory()

	state, _, _ := eng.SessionStateSnapshot()
	assert.Empty(t, state.ConversationDigest.Goals)
	assert.Empty(t, state.ConversationDigest.Decisions)
	require.NotEmpty(t, state.ConversationDigest.Excerpts)
	assert.Contains(t, state.ConversationDigest.Excerpts[0].User, "第二种")
	assert.Greater(t, state.ConversationDigest.SummaryFrontier, int64(0))
	assert.LessOrEqual(t, len(eng.messages), 1+maxHistoryMessages)
}
