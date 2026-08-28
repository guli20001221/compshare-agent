package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRefreshSystemPrompt_InjectsSelectedInstance(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("暂无用户信息")

	eng.SetSessionState(SessionState{
		SchemaVersion:        SessionStateSchemaV1,
		SelectedInstanceID:   "uhost-abc123",
		SelectedInstanceName: "my-gpu-box",
	}, 1)

	_, err := eng.ChatWithOptions(context.Background(), "hello", noopStep, ChatOptions{})
	require.NoError(t, err)

	modelInput := renderTestMessages(mock.calls[0].Messages)
	assert.Contains(t, modelInput, "my-gpu-box uhost-abc123",
		"unknown-age legacy identity must remain available for understanding")
	assert.Contains(t, modelInput, "不代表用户已经确认写操作")
	assert.Contains(t, modelInput, "用户要求实际执行时，调用适用的 Request*")
}

func TestRefreshSystemPrompt_SkipsWhenNotHydrated(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user context")

	before := eng.messages[0].Content
	_, err := eng.ChatWithOptions(context.Background(), "hello", noopStep, ChatOptions{})
	require.NoError(t, err)

	assert.Equal(t, before, eng.messages[0].Content,
		"system prompt must not change when session state is not hydrated")
}

func TestRefreshSystemPrompt_IDOnlyWhenNameEmpty(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("")

	eng.SetSessionState(SessionState{
		SchemaVersion:      SessionStateSchemaV1,
		SelectedInstanceID: "uhost-xyz789",
	}, 1)

	_, err := eng.ChatWithOptions(context.Background(), "hello", noopStep, ChatOptions{})
	require.NoError(t, err)

	modelInput := renderTestMessages(mock.calls[0].Messages)
	assert.Contains(t, modelInput, "相关对象：uhost-xyz789")
}

func TestRefreshSystemPrompt_PreservesBaseUserContext(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("您有 3 个实例（2 个运行中、1 个其他状态）")

	eng.SetSessionState(SessionState{
		SchemaVersion:        SessionStateSchemaV1,
		SelectedInstanceID:   "uhost-111",
		SelectedInstanceName: "train-node-1",
	}, 1)

	_, err := eng.ChatWithOptions(context.Background(), "hello", noopStep, ChatOptions{})
	require.NoError(t, err)

	modelInput := renderTestMessages(mock.calls[0].Messages)
	assert.True(t, strings.Contains(modelInput, "您有 3 个实例"),
		"base user context must be preserved")
	assert.True(t, strings.Contains(modelInput, "train-node-1 uhost-111"),
		"legacy session identity must be appended without restoring write trust")
}

func TestRefreshSystemPrompt_ClearsStaleInstance(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: "turn1"},
		{Content: "turn2"},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("")

	// Turn 1: load a selected instance into SessionState.
	eng.SetSessionState(SessionState{
		SchemaVersion:        SessionStateSchemaV1,
		SelectedInstanceID:   "uhost-stale",
		SelectedInstanceName: "stale-box",
	}, 1)
	_, err := eng.ChatWithOptions(context.Background(), "turn1", noopStep, ChatOptions{})
	require.NoError(t, err)
	assert.Contains(t, renderTestMessages(mock.calls[0].Messages), "uhost-stale",
		"turn 1 model context must contain selected instance")

	// Turn 2: ClearSessionState (mirrors HTTP handler flow), then
	// SetSessionState with empty instance — simulating a turn where the
	// persisted state no longer has a selected instance.
	eng.ClearSessionState()
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaV1}, 2)
	_, err = eng.ChatWithOptions(context.Background(), "turn2", noopStep, ChatOptions{})
	require.NoError(t, err)
	assert.NotContains(t, renderTestMessages(mock.calls[1].Messages), "uhost-stale",
		"turn 2 model context must NOT contain stale instance from turn 1")
	assert.NotContains(t, renderTestMessages(mock.calls[1].Messages), "stale-box",
		"turn 2 model context must NOT contain stale instance name")
}
