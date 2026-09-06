package engine

import (
	"context"
	"testing"
	"time"

	"github.com/compshare-agent/internal/actionresolver"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

// A rehydrated conversation keeps the old referent without replacing the
// Agent's new exact target.
func TestASecondTypedIDOutranksTheFirstOneItAlreadyBound(t *testing.T) {
	const first = "cpod-aaaa1111aaaa"
	for _, tc := range []struct {
		name   string
		second string
	}{
		{name: "same instance family", second: "cpod-bbbb2222bbbb"},
		{name: "different instance family", second: "uhost-bbbb2222bbbb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeInstanceOpsRunner{verdict: InstanceOpsVerdict{Text: "排查完成", Ran: 1}}
			model := &mockLLM{responses: []llm.ChatResponse{
				{ToolCalls: []openai.ToolCall{toolCall("d1", "DiagnoseInstanceInternals",
					`{"UHostId":"cpod-aaaa1111aaaa","Task":"排查 ComfyUI 打不开","Mode":"repair"}`)}},
				{ToolCalls: []openai.ToolCall{toolCall("d2", "DiagnoseInstanceInternals",
					`{"UHostId":"`+tc.second+`","Task":"排查这台","Mode":"repair"}`)}},
			}}
			eng := NewWithDeps(model, &mockExecutor{results: map[string]map[string]any{}}, nil)
			eng.SetInstanceOps(runner)
			eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)

			_, err := eng.ChatWithOptions(context.Background(), "排查 "+first+" 上的 ComfyUI", noopStep, ChatOptions{})
			require.NoError(t, err)
			require.Equal(t, first, runner.lastReq.InstanceID)

			state, _, _ := eng.SessionStateSnapshot()
			require.Equal(t, first, state.SelectedInstanceID, "the premise: the first id is now carried context")
			require.Equal(t, SelectedInstanceSourceObserved, state.SelectedInstanceSource)

			rehydrate(t, eng)
			_, err = eng.ChatWithOptions(context.Background(), tc.second, noopStep, ChatOptions{})
			require.NoError(t, err)
			require.Equal(t, tc.second, runner.lastReq.InstanceID)
			require.Equal(t, 2, runner.calls)

			state, _, _ = eng.SessionStateSnapshot()
			require.Equal(t, tc.second, state.SelectedInstanceID, "and it becomes the selection going forward")
		})
	}
}

// A new operation keeps its proposed target instead of inheriting another ID.
func TestASecondTypedIDAlsoOutranksCarriedContextForWriteWorkflows(t *testing.T) {
	const first, second = "cpod-aaaa1111aaaa", "uhost-bbbb2222bbbb"
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{map[string]any{"UHostId": second, "State": "Running"}}},
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.SetSessionState(SessionState{
		SchemaVersion:          SessionStateSchemaCurrent,
		SelectedInstanceID:     first,
		SelectedInstanceSource: SelectedInstanceSourceUser,
		SelectedInstanceAtUnix: time.Now().Unix(),
	}, 1)
	eng.lastUserMsg = "停止 " + second
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-second-target", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposal(context.Background(), map[string]any{
		"turn_id": "turn-second-target", "operation": "StopInstanceWorkflow",
		"slots": []any{map[string]any{"name": "UHostId", "value": second}},
	})

	require.NoError(t, err)
	require.True(t, resolved.action.ReadyForConfirmation, resolved.action.Rejected)
	require.Equal(t, second, resolved.action.Arguments["UHostId"])
	require.Equal(t, actionresolver.SourceUserExplicit, resolved.action.Provenance["UHostId"].Source)
}

// Names, ordinals and pronouns are resolved by the Agent. A verified concrete
// target reaches the same confirmation path for every wording.
func TestAnOrdinalTheServerCannotResolveStillReachesTheCard(t *testing.T) {
	const inferred = "uhost-2222bbbb2222"
	for _, msg := range []string{"停止第2台", "把刚才最慢的那台停掉", "停止它"} {
		t.Run(msg, func(t *testing.T) {
			executor := &mockExecutor{results: map[string]map[string]any{
				"DescribeCompShareInstance": {
					"UHostSet": []any{map[string]any{"UHostId": inferred, "State": "Running"}},
				},
			}}
			eng := NewWithDeps(&mockLLM{}, executor, nil)
			eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
			eng.lastUserMsg = msg
			eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, msg, "t-ordinal", time.Now())
			eng.turnContextViewReady = true

			resolved, err := eng.resolveActionProposal(context.Background(), map[string]any{
				"turn_id": "t-ordinal", "operation": "StopInstanceWorkflow",
				"slots": []any{map[string]any{"name": "UHostId", "value": inferred}},
			})
			require.NoError(t, err)
			require.Empty(t, resolved.action.Conflicts,
				"a reference naming no id is the Agent's to resolve; the card is where it is proven")
			require.True(t, resolved.action.ReadyForConfirmation)
		})
	}
}
