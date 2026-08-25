package engine

import (
	"context"
	"errors"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/workflow"
)

// A create that reaches the platform is irreversible and billable. Everything
// after it — the narration round, the SSE frame, the card the console draws —
// is best-effort. Measured 2026-07-29 on the real stack: the create committed
// (spot 4090, Running, billing), the closing model call died on a provider 503,
// and the user was shown 「创建实例 — 未创建成功」. The console derives that label by
// looking for an instance id in the final reply (frame/src/Frame/AIAssistant/
// formatters.js::hasCreatedInstance), so a turn that ends without one reads as
// "nothing happened" — and the user's next move is to create a second one.
func TestCommittedCreateIsRecordedWhenTheWorkflowCommits(t *testing.T) {
	exec := &recoveryMockExecutor{}
	eng := NewWithDeps(&mockLLM{}, exec, func(string, map[string]any) bool { return true })
	eng.SetSessionState(SessionState{}, 1)

	reply := eng.executeResolvedWorkflow(context.Background(),
		mustConfirmable("CreateInstanceWorkflow",
			map[string]any{"GpuType": "4090", "ImageName": "cuda128_torch291_py312"},
			zoneRefData(eng.zoneCatalogSnapshot(context.Background()))), noopStep)

	require.NotEmpty(t, eng.committedWriteRepliesThisTurn,
		"a committed create must leave a model-free record; without it no later exit can report the write")
	assert.Contains(t, eng.committedWriteRepliesThisTurn[0], "uhost-good1",
		"the record must carry the id the workflow returned, not a generic success")
	assert.Contains(t, reply, "尚未取得完整的创建后状态",
		"an optional readback miss must not be narrated as a fully delivered instance")
	assert.Contains(t, reply, "请勿重复创建")
	state, _, hydrated := eng.SessionStateSnapshot()
	require.True(t, hydrated)
	assert.Equal(t, "uhost-good1", state.SelectedInstanceID,
		"the sole result of the confirmed create must become the existing current-instance referent")
	assert.Equal(t, SelectedInstanceSourceUser, state.SelectedInstanceSource)
}

func TestCreateInstanceDeliveryReplyDistinguishesWriteFromUsableDelivery(t *testing.T) {
	tests := []struct {
		name       string
		data       map[string]any
		mustDirect bool
		contains   []string
		notContain []string
	}{
		{
			name:       "successful response omitted instance ids",
			data:       map[string]any{},
			mustDirect: true,
			contains:   []string{"没有返回实例 ID", "无法确认创建结果", "请勿重复创建"},
			notContain: []string{"✅ 已创建"},
		},
		{
			name:       "readback unavailable",
			data:       map[string]any{"UHostIds": []any{"uhost-1"}, "ActualReadbackAvailable": false},
			mustDirect: true,
			contains:   []string{"uhost-1", "尚未取得完整的创建后状态", "请勿重复创建"},
			notContain: []string{"✅ 已创建"},
		},
		{
			name: "initializing",
			data: map[string]any{
				"UHostIds": []any{"uhost-1"}, "ActualReadbackAvailable": true,
				"Observed": []map[string]any{{"UHostId": "uhost-1", "State": "Install"}},
			},
			mustDirect: true,
			contains:   []string{"当前状态为 Install", "尚未确认可用", "请勿重复创建"},
			notContain: []string{"交付成功"},
		},
		{
			name: "initialization failed",
			data: map[string]any{
				"UHostIds": []any{"uhost-1"}, "ActualReadbackAvailable": true,
				"Observed": []map[string]any{{"UHostId": "uhost-1", "State": "Install Fail"}},
			},
			mustDirect: true,
			contains:   []string{"初始化失败", "不能视为交付成功", "请勿重复创建"},
			notContain: []string{"✅ 已创建"},
		},
		{
			name: "served spec differs from confirmed card",
			data: map[string]any{
				"UHostIds": []any{"uhost-1"}, "ActualReadbackAvailable": true,
				"Observed": []map[string]any{{"UHostId": "uhost-1", "State": "Running"}},
				"SpecMismatch": []map[string]any{
					{"UHostId": "uhost-1", "Field": "CPU", "Intended": 14, "Observed": 16},
					{"UHostId": "uhost-1", "Field": "Memory", "Intended": 122880, "Observed": 98304},
				},
			},
			mustDirect: true,
			contains:   []string{"规格与确认内容不一致", "CPU 确认 14、实际 16", "内存 确认 120GB、实际 96GB", "请勿重复创建"},
			notContain: []string{"✅ 已创建"},
		},
		{
			name: "only part of a multi-create was read back",
			data: map[string]any{
				"UHostIds": []any{"uhost-1", "uhost-2"}, "ActualReadbackAvailable": true,
				"Observed": []map[string]any{{"UHostId": "uhost-1", "State": "Running"}},
			},
			mustDirect: true,
			contains:   []string{"uhost-1、uhost-2", "尚未取得完整的创建后状态"},
		},
		{
			name: "running with incomplete spec readback",
			data: map[string]any{
				"UHostIds": []any{"uhost-1"}, "ActualReadbackAvailable": true,
				"Intended": map[string]any{"CPU": 16, "Memory": 65536, "GPU": 1, "GpuType": "5090", "Zone": "cn-wlcb-01"},
				"Observed": []map[string]any{{
					"UHostId": "uhost-1", "State": "Running", "CPU": 16, "Memory": 65536,
					"GPU": 1, "GpuType": "", "Zone": "cn-wlcb-01",
				}},
			},
			mustDirect: true,
			contains:   []string{"实例已创建并处于运行中", "规格回读不完整", "请勿重复创建"},
			notContain: []string{"✅ 已创建"},
		},
		{
			name: "running and matched",
			data: map[string]any{
				"UHostIds": []any{"uhost-1"}, "ActualReadbackAvailable": true,
				"Intended": map[string]any{"CPU": 16, "Memory": 65536, "GPU": 1, "GpuType": "5090", "Zone": "cn-wlcb-01"},
				"Observed": []map[string]any{{
					"UHostId": "uhost-1", "State": "Running", "CPU": 16, "Memory": 65536,
					"GPU": 1, "GpuType": "5090", "Zone": "cn-wlcb-01",
				}},
			},
			mustDirect: false,
			contains:   []string{"✅ 已创建实例 uhost-1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reply, mustDirect := createInstanceDeliveryReply(&workflow.Result{Success: true, Data: tt.data})
			require.Equal(t, tt.mustDirect, mustDirect)
			for _, want := range tt.contains {
				assert.Contains(t, reply, want)
			}
			for _, forbidden := range tt.notContain {
				assert.NotContains(t, reply, forbidden)
			}
		})
	}
}

func TestConfirmedSingleCreateBecomesTheCurrentInstanceReferent(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{}, 1)
	result := &workflow.Result{Success: true, Data: map[string]any{
		"UHostIds": []any{"uhost-new-1"},
		"Observed": []map[string]any{{"UHostId": "uhost-new-1", "Name": "new-trainer", "State": "Install Fail"}},
	}}

	eng.recordCreatedInstanceReferent("CreateInstanceWorkflow", map[string]any{"Name": "requested-name"}, result)
	state, _, hydrated := eng.SessionStateSnapshot()

	require.True(t, hydrated)
	assert.Equal(t, "uhost-new-1", state.SelectedInstanceID)
	assert.Equal(t, "new-trainer", state.SelectedInstanceName)
	assert.Equal(t, SelectedInstanceSourceUser, state.SelectedInstanceSource)
	assert.Equal(t, ContinuityFreshnessFresh, state.SelectedInstanceFreshness)
}

func TestMultiInstanceCreateDoesNotChooseAReferent(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{}, 1)
	eng.recordCreatedInstanceReferent("CreateInstanceWorkflow", nil, &workflow.Result{Success: true, Data: map[string]any{
		"UHostIds": []any{"uhost-new-1", "uhost-new-2"},
	}})

	state, _, _ := eng.SessionStateSnapshot()
	assert.Empty(t, state.SelectedInstanceID)
}

// The recovery text has two jobs: name the instance (so the console's id probe
// finds it and the card stops saying 未创建成功) and say the summary is missing
// (so the silence is not read as "the assistant had nothing to add").
func TestCommittedWriteRecoveryReplyNamesTheInstanceAndWarnsAgainstResubmitting(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.committedWriteRepliesThisTurn = []string{"✅ 已创建实例 uhost-good1。"}

	reply, ok := eng.committedWriteRecoveryReply()

	require.True(t, ok)
	assert.Contains(t, reply, "uhost-good1")
	assert.Contains(t, reply, "请勿重复提交")
	assert.NotContains(t, reply, "未创建")
}

// The recovery must not fire on a turn that wrote nothing: an LLM outage during
// a read turn is a genuine failure and has to surface as one. Turning every
// model error into a confirmation would be a far worse bug than the one this
// fixes.
func TestCommittedWriteRecoveryStaysSilentWhenNothingCommitted(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)

	reply, ok := eng.committedWriteRecoveryReply()

	assert.False(t, ok, "no committed write → no recovery reply")
	assert.Empty(t, reply)
}

// The record is turn-scoped, and it has to be: an LLM outage on the NEXT turn
// must not be answered with the previous turn's instance id. That failure would
// be worse than the bug being fixed — a fabricated creation the user never
// requested, carrying a real id from their own account so it looks credible.
func TestCommittedWriteRecordDoesNotSurviveIntoTheNextTurn(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "第一轮回复"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}
	eng.committedWriteRepliesThisTurn = []string{"✅ 已创建实例 uhost-good1。"}

	_, err := eng.Chat(context.Background(), "上一轮之后的新问题", noopStep)

	require.NoError(t, err)
	assert.Empty(t, eng.committedWriteRepliesThisTurn,
		"a new turn starts with no committed writes; carrying them forward invents a creation")
}

// committedThenFailingLLM plays the two rounds the outage straddles: round 1
// issues a tool call and — standing in for the write that round's workflow
// executed — leaves the commit record the real path leaves (pinned against the
// real workflow by TestCommittedCreateIsRecordedWhenTheWorkflowCommits); round 2
// is the narration call that never lands.
type committedThenFailingLLM struct {
	eng   *Engine
	calls int
	err   error
}

func (m *committedThenFailingLLM) Chat(_ context.Context, _ llm.ChatRequest) (*llm.ChatResponse, error) {
	m.calls++
	if m.calls == 1 {
		m.eng.committedWriteRepliesThisTurn = append(m.eng.committedWriteRepliesThisTurn, "✅ 已创建实例 uhost-good1。")
		return &llm.ChatResponse{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "ReadCapability_resource_info", `{}`),
		}}, nil
	}
	return nil, m.err
}

// The turn must end as an ANSWER, not an error. This is the whole point: the
// write outlives the model, so the model's failure may not be reported as the
// write's failure.
func TestChatReportsTheCommittedWriteWhenTheNarrationCallFails(t *testing.T) {
	mock := &committedThenFailingLLM{err: errors.New("llm stream: error, status code: 503, status: 503 Service Unavailable, message: No available accounts.")}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	mock.eng = eng
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}

	var streamed string
	reply, err := eng.ChatWithOptions(context.Background(), "为我开一台抢占式的4090", noopStep, ChatOptions{
		OnTextDelta: func(s string) { streamed += s },
	})

	require.NoError(t, err, "a committed write must not be reported as a failed turn")
	assert.Contains(t, reply, "uhost-good1")
	assert.Contains(t, reply, "请勿重复提交")
	assert.Equal(t, reply, streamed, "the client must receive exactly the persisted reply")
	require.NotEmpty(t, eng.messages)
	assert.Equal(t, reply, eng.messages[len(eng.messages)-1].Content,
		"history must keep the reply, so a follow-up turn knows the instance exists")
}

// committedThenTruncatedLLM is the same committed-write shape as the provider
// outage above, except the narration call reaches EOF with finish_reason=length.
// The third response exists only to make a regression observable: the recovery
// must finish after two calls, rather than spending a retry after the write has
// already committed.
type committedThenTruncatedLLM struct {
	eng   *Engine
	calls int
}

func (m *committedThenTruncatedLLM) Chat(_ context.Context, _ llm.ChatRequest) (*llm.ChatResponse, error) {
	m.calls++
	if m.calls == 1 {
		m.eng.committedWriteRepliesThisTurn = append(m.eng.committedWriteRepliesThisTurn, "✅ 已创建实例 uhost-good1。")
		return &llm.ChatResponse{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "ReadCapability_resource_info", `{}`),
		}}, nil
	}
	if m.calls == 2 {
		return &llm.ChatResponse{Content: "这段截断的说明不能作为最终回复", StopReason: "length"}, nil
	}
	return &llm.ChatResponse{Content: "不应执行这次重试"}, nil
}

func TestChatReportsCommittedWriteWhenNarrationIsLengthStopped(t *testing.T) {
	mock := &committedThenTruncatedLLM{}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	mock.eng = eng
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}

	var streamed string
	reply, err := eng.ChatWithOptions(context.Background(), "为我开一台抢占式的4090", noopStep, ChatOptions{
		OnTextDelta: func(s string) { streamed += s },
	})

	require.NoError(t, err)
	require.Contains(t, reply, "uhost-good1")
	require.Contains(t, reply, "请勿重复提交")
	require.NotContains(t, reply, "这段截断的说明")
	require.Equal(t, 2, mock.calls,
		"an already-committed write must be reported before spending a truncation retry")
	require.Equal(t, reply, streamed, "the client must receive exactly the persisted recovery reply")
	require.Equal(t, reply, eng.messages[len(eng.messages)-1].Content)
}
