package engine

import (
	"context"
	"encoding/json"
	"fmt"
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
	assert.NotContains(t, string(raw), "conversation_digest",
		"the digest is deleted, not merely unrendered: it must not reappear on the wire")
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
			SlotSources:     map[string]string{"instance_id": "user"},
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
	assert.Equal(t, "把训练机的数据盘扩到 200G", state.TaskSnapshot.Goal)
	assert.Equal(t, "missing_slots", state.TaskSnapshot.Stage)
	assert.Equal(t, []string{"disk_id"}, state.TaskSnapshot.MissingSlots)
	assert.Equal(t, TaskSnapshotStatusExpired, state.TaskSnapshot.Status)
	assert.Contains(t, renderTestMessages(eng.llmClient.(*mockLLM).calls[0].Messages), "新鲜度=expired")
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
	modelInput := renderTestMessages(eng.llmClient.(*mockLLM).calls[0].Messages)
	assert.Contains(t, modelInput, "当前值必须重新查询")
	assert.NotContains(t, modelInput, "SECRET_OLD_99")
}

func TestTrimHistory_RemovesRawToolTranscriptAndKeepsSemanticSignalsInCard(t *testing.T) {
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

	// The structured task signals cross the turn boundary through the single
	// context card; raw tool JSON never does.
	now := time.Now()
	card := renderAgentContextCard((ContextCompiler{}).Compile(eng, "继续扩盘", now))
	assert.Contains(t, card, "把训练机的数据盘扩到 200G")
	// instance=uhost-a and target_size_gb=200 reached the card through the digest's
	// 既有约束 / 已作决定 blocks. Those blocks and the merge that fed them are both
	// deleted; the transcript replays the turn that produced them instead. The
	// task's own line is what the card still keeps.
	assert.NotContains(t, card, "instance=uhost-a")
	assert.NotContains(t, card, "target_size_gb=200")
	assert.Contains(t, card, "disk_id")
	assert.NotContains(t, card, "MUST_NOT_SURVIVE")
}

// Trimming discards. It used to distil evicted turns into ConversationDigest, and
// the test here asserted that the excerpts survived; both the digest and that
// hand-off are gone.
//
// What replaces the assertion is the reason the hand-off was not load-bearing:
// the model's cross-turn memory is what maxReplayedHistoryRunes admits, and the
// raw list is budgeted ABOVE that (maxRawHistoryRunes), so an exchange can only
// leave the model's view by losing the replay budget — never by this trim.
//
// That used to be an argument about two counts ("maxHistoryMessages/2 pairs
// survive against a maxAgentContextPairs window"), which held only for exchanges
// of an assumed size. Both counts are gone and the relation is now between two
// budgets in the same unit, so the assertion below has real content: lowering
// maxRawHistoryRunes under maxReplayedHistoryRunes fails it.
//
// BOTH branches are run. trimHistoryWithContext forks on
// reactHistoryCompactionEnabled and only one side lives in history_compaction.go;
// a first version of this test exercised the other one, and a mutation that
// lowered the compaction ceiling squarely into the replay window passed it. A
// trim test that never reaches the trim under test is the same empty gate as no
// test.
func TestTrimmingNeverReachesTheReplayWindow(t *testing.T) {
	for _, compaction := range []bool{false, true} {
		t.Run(fmt.Sprintf("compaction=%v", compaction), func(t *testing.T) {
			eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
			eng.SetReactHistoryCompactionEnabled(compaction)
			eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)

			// Comfortably over the RAW ceiling, so the trim has to cut, and far enough
			// over the replay budget that the replayed window is a strict subset of
			// what survives the cut.
			const padRunes = 400
			pairs := 2 * overflowingPairs(padRunes)
			eng.messages = makePaddedHistory(pairs, padRunes)
			require.Greater(t, assembledRequestRunes(eng.messages[1:]), maxRawHistoryRunes,
				"premise: the input must overflow the raw ceiling")

			before := eng.recentCompleteConversationPairs()
			require.Greater(t, len(before), 0, "premise: something is replayed at all")
			require.Less(t, len(before), pairs,
				"premise: the replay budget must itself be biting, or this compares two untrimmed lists")

			eng.trimHistory()

			require.Less(t, len(eng.messages), 1+2*pairs,
				"the trim must actually have fired, or the comparison below is between two identical values")
			assert.Equal(t, before, eng.recentCompleteConversationPairs(),
				"the trim changed what the model reads; maxRawHistoryRunes must stay above "+
					"maxReplayedHistoryRunes so the raw list is never the narrower of the two")
		})
	}
}
