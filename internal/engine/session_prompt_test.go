package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/intent"
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

	sysPrompt := eng.messages[0].Content
	assert.Contains(t, sysPrompt, "当前会话已选实例：my-gpu-box（uhost-abc123）",
		"system prompt must contain selected instance after refreshSystemPrompt")
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

	sysPrompt := eng.messages[0].Content
	assert.Contains(t, sysPrompt, "当前会话已选实例：uhost-xyz789")
	assert.NotContains(t, sysPrompt, "（uhost-xyz789）",
		"no parenthetical ID when name is empty — ID is the primary label")
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

	sysPrompt := eng.messages[0].Content
	assert.True(t, strings.Contains(sysPrompt, "您有 3 个实例"),
		"base user context must be preserved")
	assert.True(t, strings.Contains(sysPrompt, "当前会话已选实例：train-node-1（uhost-111）"),
		"session state must be appended")
}

func TestPlannerInput_ReceivesLastIntent(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{
		Plan: unknownEngineTestPlan(),
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		Model:          "test",
		EnabledIntents: []intent.Intent{intent.IntentResourceInfo},
	})

	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaV1,
		LastIntent:    "resource_info",
	}, 1)

	_, err := eng.ChatWithOptions(context.Background(), "重启它", noopStep, ChatOptions{})
	require.NoError(t, err)
	require.Len(t, planner.calls, 1)

	assert.Equal(t, "resource_info", planner.calls[0].LastIntent,
		"planner must receive LastIntent from session state")
	assert.Equal(t, "重启它", planner.calls[0].UserText)
}

func TestPlannerInput_LastIntentEmptyWhenNotHydrated(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{
		Plan: unknownEngineTestPlan(),
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		Model:          "test",
		EnabledIntents: []intent.Intent{intent.IntentResourceInfo},
	})

	_, err := eng.ChatWithOptions(context.Background(), "hello", noopStep, ChatOptions{})
	require.NoError(t, err)
	require.Len(t, planner.calls, 1)

	assert.Empty(t, planner.calls[0].LastIntent,
		"planner LastIntent must be empty when session state not hydrated")
}

func TestRefreshSystemPrompt_ClearsStaleInstance(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: "turn1"},
		{Content: "turn2"},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("")

	// Turn 1: inject selected instance via SessionState.
	eng.SetSessionState(SessionState{
		SchemaVersion:        SessionStateSchemaV1,
		SelectedInstanceID:   "uhost-stale",
		SelectedInstanceName: "stale-box",
	}, 1)
	_, err := eng.ChatWithOptions(context.Background(), "turn1", noopStep, ChatOptions{})
	require.NoError(t, err)
	assert.Contains(t, eng.messages[0].Content, "uhost-stale",
		"turn 1 system prompt must contain selected instance")

	// Turn 2: ClearSessionState (mirrors HTTP handler flow), then
	// SetSessionState with empty instance — simulating a turn where the
	// persisted state no longer has a selected instance.
	eng.ClearSessionState()
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaV1}, 2)
	_, err = eng.ChatWithOptions(context.Background(), "turn2", noopStep, ChatOptions{})
	require.NoError(t, err)
	assert.NotContains(t, eng.messages[0].Content, "uhost-stale",
		"turn 2 system prompt must NOT contain stale instance from turn 1")
	assert.NotContains(t, eng.messages[0].Content, "stale-box",
		"turn 2 system prompt must NOT contain stale instance name")
}
