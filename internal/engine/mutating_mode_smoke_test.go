package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/llm"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMutatingModeSmoke_L1WorkflowStopsAtConfirmWhenDenied(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-stop-001", "State": "Running", "GpuType": "4090", "Name": "test"},
			},
		},
		"StopCompShareInstance": {"RetCode": 0},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "StopInstanceWorkflow", `{"UHostId":"uhost-stop-001"}`),
		}},
		{Content: "cancelled"},
	}}
	confirmCalls := 0
	eng := NewWithDeps(mock, executor, func(string, map[string]any) bool {
		confirmCalls++
		return false
	})
	eng.SetMutatingToolsEnabled(true)
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}
	onStep, events := collectSteps()

	reply, err := eng.Chat(context.Background(), "stop uhost-stop-001", onStep)

	require.NoError(t, err)
	assert.Contains(t, reply, "未执行")
	assert.NotContains(t, reply, "已取消", "a not-granted confirm must not falsely claim the user cancelled")
	assert.Equal(t, 1, confirmCalls, "L1 workflow must ask for exactly one confirmation")
	assertStepWithType(t, *events, StepConfirmNeeded, "", "")
	assert.Contains(t, executor.calls, "DescribeCompShareInstance")
	assert.NotContains(t, executor.calls, "StopCompShareInstance",
		"denied confirmation must stop before the mutating API call")
}

func TestMutatingModeSmoke_CreateCustomImageStopsAtConfirmWhenDenied(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-img-001", "State": "Running", "Name": "train-env"},
			},
		},
		"CreateCompShareCustomImage":      {"CompShareImageId": "cimg-custom-001"},
		"GetCompShareImageCreateProgress": {"Process": float64(10)},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "CreateCustomImageWorkflow", `{"UHostId":"uhost-img-001","Name":"snapshot-v1"}`),
		}},
		{Content: "cancelled"},
	}}
	confirmCalls := 0
	eng := NewWithDeps(mock, executor, func(string, map[string]any) bool {
		confirmCalls++
		return false
	})
	eng.SetMutatingToolsEnabled(true)
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}
	onStep, events := collectSteps()

	reply, err := eng.Chat(context.Background(), "save uhost-img-001 as snapshot-v1", onStep)

	require.NoError(t, err)
	assert.Contains(t, reply, "未执行")
	assert.NotContains(t, reply, "已取消", "a not-granted confirm must not falsely claim the user cancelled")
	assert.Equal(t, 1, confirmCalls, "custom-image workflow must ask for exactly one confirmation")
	assertStepWithType(t, *events, StepConfirmNeeded, "", "")
	assert.Contains(t, executor.calls, "DescribeCompShareInstance")
	assert.NotContains(t, executor.calls, "CreateCompShareCustomImage",
		"denied confirmation must stop before the mutating API call")
}

func TestMutatingModeSmoke_DestructiveActionBlockedEvenWhenWritesEnabled(t *testing.T) {
	executor := &mockExecutor{}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "TerminateCompShareInstance", `{"UHostId":"uhost-del-001"}`),
		}},
		{Content: "blocked"},
	}}
	confirmCalls := 0
	eng := NewWithDeps(mock, executor, func(string, map[string]any) bool {
		confirmCalls++
		return true
	})
	eng.SetMutatingToolsEnabled(true)
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}
	onStep, events := collectSteps()

	reply, err := eng.Chat(context.Background(), "delete uhost-del-001", onStep)

	require.NoError(t, err)
	assert.Contains(t, reply, "已拒绝")
	assert.Equal(t, 0, confirmCalls, "destructive actions must be blocked before confirmation")
	assertStepWithType(t, *events, StepBlocked, "TerminateCompShareInstance", "")
	assertNoStepTypeForAction(t, *events, StepConfirmNeeded, "TerminateCompShareInstance")
	assert.NotContains(t, executor.calls, "TerminateCompShareInstance")
}
