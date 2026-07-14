package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/llm"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// "Did anything actually HAPPEN this turn?"
//
// The HTTP layer asks this when a turn cannot be saved, and it is the only thing
// standing between a user whose instance was just created and the sentence "本轮
// 未保存，请重试" — which, to that user, means "create a second one".
//
// The errors are not symmetric, and the engine is written accordingly:
//
//	a false "something happened"  -> the user re-asks once. Annoying.
//	a false "nothing happened"    -> the user retries a create. Billed twice.
//
// So it errs toward "something happened", and the boundary it draws is the
// CONFIRMATION: everything a workflow does to the platform happens after the user
// grants it. A workflow that was refused at that gate changed nothing.
//
// My first attempt at this recorded in executeSafeTool — and was silently wrong,
// because every mutating action in this codebase is a *Workflow tool
// (tools/policies.go: L1 == a name ending in "Workflow") and workflows do not go
// through executeSafeTool at all. It recorded nothing, ever. The httpapi gate is
// what caught it.
// ---------------------------------------------------------------------------

func instanceExecutor() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-1exampleaa0N", "State": "Stopped", "GpuType": "4090", "Name": "demo"},
			},
		},
		"StartCompShareInstance": {"RetCode": 0},
		"StopCompShareInstance":  {"RetCode": 0},
	}}
}

func workflowTurn(t *testing.T, tool, args string, confirm bool) *Engine {
	t.Helper()
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("tc1", tool, args)}},
		{Content: "done"},
	}}
	eng := NewWithDeps(mock, instanceExecutor(), func(string, map[string]any) bool { return confirm })
	eng.SetMutatingToolsEnabled(true)
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}
	_, err := eng.Chat(context.Background(), "开机 uhost-1exampleaa0N", noopStep)
	require.NoError(t, err)
	return eng
}

// A workflow the user CONFIRMED and that ran is something that happened.
//
// Mutation: delete the append in executeWorkflow and this fails — after which the
// HTTP layer would tell a user whose instance was just started to "retry".
func TestMutatingActionsThisTurn_AConfirmedWorkflowIsRecorded(t *testing.T) {
	eng := workflowTurn(t, "StartInstanceWorkflow", `{"UHostId":"uhost-1exampleaa0N"}`, true)

	assert.Equal(t, []string{"StartInstanceWorkflow"}, eng.MutatingActionsThisTurn(),
		"the platform was changed; a turn that then fails to save must not offer a plain retry")
}

// A workflow the user REFUSED changed nothing, and must not be recorded.
//
// This is the negative control, and without it "always record every workflow the
// model asked for" would pass the test above. Under that rule every declined
// confirmation would start telling users NOT to retry a turn in which, in fact,
// nothing at all had happened — turning a safety feature into noise, which is how
// safety features get switched off.
func TestMutatingActionsThisTurn_ARefusedWorkflowIsNotRecorded(t *testing.T) {
	eng := workflowTurn(t, "StopInstanceWorkflow", `{"UHostId":"uhost-1exampleaa0N"}`, false)

	assert.Empty(t, eng.MutatingActionsThisTurn(),
		"the user said no, so nothing reached the platform — this turn is safe to retry")
}

// A read-only turn records nothing.
func TestMutatingActionsThisTurn_AReadOnlyTurnRecordsNothing(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("tc1", "DescribeCompShareInstance", `{}`)}},
		{Content: "你有一台实例"},
	}}
	eng := NewWithDeps(mock, instanceExecutor(), nil)
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}

	_, err := eng.Chat(context.Background(), "我有几台机器", noopStep)
	require.NoError(t, err)

	assert.Empty(t, eng.MutatingActionsThisTurn())
}

// The record is per-turn, not cumulative. A pooled engine serves many turns; if
// turn N's create leaked into turn N+1, a read-only question that failed to save
// would refuse to offer a retry for no reason.
func TestMutatingActionsThisTurn_IsResetBetweenTurns(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("tc1", "StartInstanceWorkflow", `{"UHostId":"uhost-1exampleaa0N"}`)}},
		{Content: "已开机"},
		{Content: "4090 有 24G 显存"},
	}}
	eng := NewWithDeps(mock, instanceExecutor(), func(string, map[string]any) bool { return true })
	eng.SetMutatingToolsEnabled(true)
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}

	// The instance must be named in the user's own words: workflowTargetIsTrusted refuses a
	// mutating target the user never asked for, which is a separate (and correct) guard.
	_, err := eng.Chat(context.Background(), "开机 uhost-1exampleaa0N", noopStep)
	require.NoError(t, err)
	require.NotEmpty(t, eng.MutatingActionsThisTurn(), "precondition: turn 1 started an instance")

	_, err = eng.Chat(context.Background(), "4090 显存多大", noopStep)
	require.NoError(t, err)

	assert.Empty(t, eng.MutatingActionsThisTurn(),
		"turn 2 changed nothing — the previous turn's action must not still be recorded")
}
