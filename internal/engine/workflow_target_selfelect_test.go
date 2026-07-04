package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/tools"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExecuteWorkflowBlocksModelSelfElectedTargetViaSingleDescribe is the
// regression pin for the mid-turn self-election bypass: workflowTargetIsTrusted
// must NOT trust a target the MODEL elected mid-turn via a single-host describe.
//
// Chain: a single-host DescribeCompShareInstance issued by the model in ReAct is
// an OriginDirectLLM tool call (plannerHandlerExecutor.Execute, engine.go:3567)
// -> recordToolFacts (engine.go:4290) -> recordInstanceStateFacts (len==1,
// engine.go:4549) -> recordSelectedInstanceID sets sessionState.SelectedInstanceID
// (engine.go:4821). The guard's live-field branch (engine.go:5420) then treats
// that model-written value as user-intended and lets the stop through.
//
// staleStateNote (engine.go:6059) actively instructs the model to call
// DescribeCompShareInstance before executing an instance change, so describe-one-
// then-stop is a realistic flash path, not a contrived one.
func TestExecuteWorkflowBlocksModelSelfElectedTargetViaSingleDescribe(t *testing.T) {
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{
				map[string]any{"UHostId": "uhost-a", "Name": "alpha", "State": "Running", "Zone": "cn-wlcb-01"},
			}}, nil
		case "StopCompShareInstance":
			return map[string]any{"RetCode": 0}, nil
		default:
			return map[string]any{"RetCode": 0}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{}, exec, func(string, map[string]any) bool { return true })
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	eng.lastUserMsg = "怎么关机"               // zero-target request on a multi-instance account
	eng.selectedInstanceIDAtTurnStart = "" // nothing selected before this turn
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"TotalCount": float64(2),
		"UHostSet": []any{
			map[string]any{"UHostId": "uhost-a", "Name": "alpha", "State": "Running"},
			map[string]any{"UHostId": "uhost-b", "Name": "beta", "State": "Running"},
		},
	}, "test"))

	// Model describes the ONE instance it chose (nudged by staleStateNote).
	eng.recordToolFacts("DescribeCompShareInstance", map[string]any{"UHostId": "uhost-a"}, &tools.SafeToolResult{
		RawResult: map[string]any{"UHostSet": []any{
			map[string]any{"UHostId": "uhost-a", "Name": "alpha", "State": "Running"},
		}},
	})
	require.Equal(t, "uhost-a", eng.sessionState.SelectedInstanceID,
		"precondition: a single-host describe self-elects the target mid-turn")
	require.Equal(t, SelectedInstanceSourceObserved, eng.sessionState.SelectedInstanceSource,
		"precondition: single-host tool observations must not be trusted as user-selected targets")

	reply := eng.executeWorkflow(context.Background(), "StopInstanceWorkflow", map[string]any{"UHostId": "uhost-a"}, noopStep)

	// The user never referenced uhost-a; the model picked it. It must be blocked.
	assert.Contains(t, reply, "请先确认要操作的实例",
		"model self-elected target (via single describe) must NOT be trusted")
	assert.Empty(t, exec.calls, "self-elected stop must be blocked before workflow tools run")
}

func TestChatBlocksModelSelfElectedTargetViaSingleDescribe(t *testing.T) {
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{
				map[string]any{"UHostId": "uhost-a", "Name": "alpha", "State": "Running", "Zone": "cn-wlcb-01"},
			}}, nil
		case "StopCompShareInstance":
			return map[string]any{"RetCode": 0}, nil
		default:
			return map[string]any{"RetCode": 0}, nil
		}
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc-describe", "DescribeCompShareInstance", `{"UHostIds":["uhost-a"]}`),
		}},
		{ToolCalls: []openai.ToolCall{
			toolCall("tc-stop", "StopInstanceWorkflow", `{"UHostId":"uhost-a"}`),
		}},
		{Content: "请先确认要操作的实例。"},
	}}
	confirmCalls := 0
	eng := NewWithDeps(mock, exec, func(string, map[string]any) bool {
		confirmCalls++
		return true
	})
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"TotalCount": float64(2),
		"UHostSet": []any{
			map[string]any{"UHostId": "uhost-a", "Name": "alpha", "State": "Running"},
			map[string]any{"UHostId": "uhost-b", "Name": "beta", "State": "Running"},
		},
	}, "test"))
	onStep, events := collectSteps()

	reply, err := eng.Chat(context.Background(), "怎么关机", onStep)

	require.NoError(t, err)
	assert.Contains(t, reply, "请先确认要操作的实例")
	assert.Contains(t, exec.calls, "DescribeCompShareInstance")
	assert.NotContains(t, exec.calls, "StopCompShareInstance")
	assert.Equal(t, 0, confirmCalls, "self-elected target must be blocked before confirmation")
	assertStepWithType(t, *events, StepBlocked, "StopInstanceWorkflow", "请先确认")
}
