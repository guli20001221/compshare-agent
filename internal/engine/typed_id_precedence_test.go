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

// The follow-on shape of TestATypedIDIsADesignationEvenWhenTheRegistryIsEmpty:
// the designation that test records is exactly what broke the NEXT one.
//
// Once A is carried as a user selection, a cold registry cannot tokenize a newly
// typed B. Tier B therefore used to return A before the caller could compare B
// with the user's literal words. Supplying the proposed target to the binder makes
// that exact literal an unresolved explicit reference: it suppresses A, while the
// existing point-query remains responsible for B.
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
			require.Equal(t, SelectedInstanceSourceUser, state.SelectedInstanceSource)

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

func TestAProposedLiteralSuppressesCarriedContextWhenTheRegistryDoesNotKnowItsPrefix(t *testing.T) {
	const first, second = "cpod-aaaa1111aaaa", "uhost-bbbb2222bbbb"
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	eng.recordSelectedInstanceIDWithSource(first, "old", SelectedInstanceSourceUser)
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"TotalCount": float64(1),
		"UHostSet":   []any{map[string]any{"UHostId": first, "Name": "old", "State": "Running"}},
	}, "test"))

	view := (ContextCompiler{}).CompileForTurn(eng, "停止 "+second, "turn-new-family", time.Now())
	binding := eng.bindInstanceTarget(view, second)

	require.True(t, binding.explicit)
	require.False(t, binding.bound(), "the old carried instance must not replace the literal target")
}

// The same binding feeds every write workflow before provenance is derived. A
// current literal target must therefore suppress the carried target there too,
// rather than being overwritten before the point-query sees it.
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

func TestAWriteWorkflowCannotUseTheCarriedTargetWhenTheUserNamesAnother(t *testing.T) {
	const carried = "cpod-aaaa1111aaaa"
	for _, current := range []string{"cpod-bbbb2222bbbb", "uhost-bbbb2222bbbb"} {
		t.Run(current, func(t *testing.T) {
			executor := &mockExecutor{results: map[string]map[string]any{
				"DescribeCompShareInstance": {
					"UHostSet": []any{map[string]any{"UHostId": carried, "State": "Running"}},
				},
			}}
			eng := NewWithDeps(&mockLLM{}, executor, nil)
			eng.SetSessionState(SessionState{
				SchemaVersion:          SessionStateSchemaCurrent,
				SelectedInstanceID:     carried,
				SelectedInstanceSource: SelectedInstanceSourceUser,
				SelectedInstanceAtUnix: time.Now().Unix(),
			}, 1)
			eng.lastUserMsg = "停止 " + current
			eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-new-target", time.Now())
			eng.turnContextViewReady = true

			resolved, err := eng.resolveActionProposal(context.Background(), map[string]any{
				"turn_id": "turn-new-target", "operation": "StopInstanceWorkflow",
				"slots": []any{map[string]any{"name": "UHostId", "value": carried}},
			})

			require.NoError(t, err)
			require.False(t, resolved.action.ReadyForConfirmation,
				"the model must not turn a current reference to B into a confirmation card for carried A")
			require.NotEmpty(t, resolved.action.Conflicts)
		})
	}
}

// A proposed target only helps the binder when the current message contains that
// exact ID. An ID invented by the model still cannot authorize entry.
func TestAModelInventedIDIsStillRefusedWhenTheBinderSeesProposals(t *testing.T) {
	const typed, invented = "cpod-aaaa1111aaaa", "cpod-9999invented9"
	runner := &fakeInstanceOpsRunner{verdict: InstanceOpsVerdict{Text: "不应到达", Ran: 1}}
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("d1", "DiagnoseInstanceInternals",
			`{"UHostId":"cpod-aaaa1111aaaa","Task":"排查 ComfyUI","Mode":"repair"}`)}},
		{ToolCalls: []openai.ToolCall{toolCall("d2", "DiagnoseInstanceInternals",
			`{"UHostId":"cpod-9999invented9","Task":"排查这台","Mode":"repair"}`)}},
		{Content: "请确认要排查哪台实例。"},
	}}
	eng := NewWithDeps(model, &mockExecutor{results: map[string]map[string]any{}}, nil)
	eng.SetInstanceOps(runner)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)

	_, err := eng.ChatWithOptions(context.Background(), "排查 cpod-aaaa1111aaaa 上的 ComfyUI", noopStep, ChatOptions{})
	require.NoError(t, err)

	rehydrate(t, eng)
	_, err = eng.ChatWithOptions(context.Background(), "再看看 cpod-aaaa1111aaaa 的显存", noopStep, ChatOptions{})
	require.NoError(t, err)
	require.Equal(t, typed, runner.lastReq.InstanceID, "and must not be the instance entered")

	state, _, _ := eng.SessionStateSnapshot()
	require.Equal(t, typed, state.SelectedInstanceID, "nor become the selection")
	require.Equal(t, SelectedInstanceSourceUser, state.SelectedInstanceSource)
	require.NotEqual(t, invented, state.SelectedInstanceID)
}

// When the model repeats carried A after the user types B, the ID-shaped-token
// signal must suppress A even though the proposal signal cannot see B.
func TestACarriedTargetTheUserDidNotNameThisTurnDoesNotAuthorizeEntry(t *testing.T) {
	const carried = "cpod-aaaa1111aaaa"
	for _, second := range []string{"cpod-bbbb2222bbbb", "uhost-bbbb2222bbbb"} {
		t.Run(second, func(t *testing.T) {
			runner := &fakeInstanceOpsRunner{verdict: InstanceOpsVerdict{Text: "排查完成", Ran: 1}}
			model := &mockLLM{responses: []llm.ChatResponse{
				{ToolCalls: []openai.ToolCall{toolCall("d1", "DiagnoseInstanceInternals",
					`{"UHostId":"cpod-aaaa1111aaaa","Task":"排查 ComfyUI","Mode":"repair"}`)}},
				{ToolCalls: []openai.ToolCall{toolCall("d2", "DiagnoseInstanceInternals",
					`{"UHostId":"cpod-aaaa1111aaaa","Task":"排查这台","Mode":"repair"}`)}},
				{Content: "请确认要排查哪台实例。"},
			}}
			eng := NewWithDeps(model, &mockExecutor{results: map[string]map[string]any{}}, nil)
			eng.SetInstanceOps(runner)
			eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)

			_, err := eng.ChatWithOptions(context.Background(), "排查 "+carried+" 上的 ComfyUI", noopStep, ChatOptions{})
			require.NoError(t, err)

			rehydrate(t, eng)
			_, err = eng.ChatWithOptions(context.Background(), "现在排查 "+second+" 上的 ComfyUI", noopStep, ChatOptions{})
			require.NoError(t, err)
			require.Equal(t, 1, runner.calls, "and must not be entered")
		})
	}
}

// The target adjudicator's new conflict arm must separate "the user wrote an id"
// from "the user pointed at something". An out-of-list ordinal sets explicit and
// names no id, so gating on explicit stopped 「停止第2台」 from ever reaching a
// confirmation card — measured: ready=true before the arm existed, a
// 目标引用不唯一 conflict after. That is the flow the binder's own contract hands
// to the Agent on purpose: a reference the server cannot prove rides into the
// card, where the user's confirm is the proof.
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

// The control on the arm above: when the user DID write an id and the model
// proposes a different one, that is a disagreement and must still stop.
func TestAWrittenIDTheModelContradictsStillConflicts(t *testing.T) {
	const written, inferred = "uhost-1111aaaa1111", "uhost-2222bbbb2222"
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{map[string]any{"UHostId": inferred, "State": "Running"}},
		},
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	eng.lastUserMsg = "停止 " + written
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "t-written", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposal(context.Background(), map[string]any{
		"turn_id": "t-written", "operation": "StopInstanceWorkflow",
		"slots": []any{map[string]any{"name": "UHostId", "value": inferred}},
	})
	require.NoError(t, err)
	require.NotEmpty(t, resolved.action.Conflicts)
	require.False(t, resolved.action.ReadyForConfirmation)
}
