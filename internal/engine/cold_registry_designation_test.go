package engine

import (
	"context"
	"testing"
	"time"

	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

// The same production loop as user_designated_instance_test.go, at the registry
// state production actually runs in.
//
// #566 recorded the designation at turn entry from bindInstanceTarget, which finds
// a typed id through RegistrySnapshot.InstanceIDTokensInText — and that returns nil
// on an EMPTY snapshot, because its id prefixes are derived from the instances the
// snapshot already holds (entity/resolve.go:189-197). HTTP sessions skip
// engine.Init(), so a session that has not listed instances carries exactly that
// empty snapshot for its whole life, and the shape this lane exists for lists
// nothing: 「ComfyUI 打不开」, then a bare 「cpod-…」, straight into the lane.
//
// So on 2026-08-17, with #566 already deployed, the loop came back: the entry card
// was shown (the target gate has its own last-resort check on the user's literal
// words), the card timed out, nothing was recorded, and the next 「继续排查」 was
// refused — again with nothing the user could say that would clear it.
func TestATypedIDIsADesignationEvenWhenTheRegistryIsEmpty(t *testing.T) {
	const instanceID = "cpod-typed-1"
	runner := &fakeInstanceOpsRunner{verdict: InstanceOpsVerdict{Text: "排查完成", Ran: 1}}
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("d1", "DiagnoseInstanceInternals",
			`{"UHostId":"cpod-typed-1","Task":"排查 ComfyUI 打不开"}`)}},
		{Content: "授权卡片已超时，本次没有进入实例。"},
		{ToolCalls: []openai.ToolCall{toolCall("d2", "DiagnoseInstanceInternals",
			`{"UHostId":"cpod-typed-1","Task":"继续排查 ComfyUI"}`)}},
	}}
	eng := NewWithDeps(model, &mockExecutor{results: map[string]map[string]any{}}, nil)
	eng.SetInstanceOps(runner)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	// Deliberately NO registry sync: this is the never-synced snapshot a fresh HTTP
	// session carries, not a contrived state.
	require.True(t, eng.registry.NeedsRefresh(time.Now()), "the premise is a cold registry")

	cards := 0
	opts := ChatOptions{ConfirmResultFunc: func(_ string, args map[string]any) ConfirmationResult {
		cards++
		if cards == 1 {
			return ConfirmationResult{Confirmed: false, TerminalReason: observability.ConfirmationReasonTimeout}
		}
		require.Equal(t, instanceID, args["UHostId"],
			"the replacement card must name the same instance the user typed")
		return ConfirmationResult{Confirmed: true}
	}}

	_, err := eng.ChatWithOptions(context.Background(),
		"排查 cpod-typed-1 上的 ComfyUI", noopStep, opts)
	require.NoError(t, err)
	require.Zero(t, runner.calls, "a card that timed out never enters the instance")

	state, _, _ := eng.SessionStateSnapshot()
	require.Equal(t, instanceID, state.SelectedInstanceID,
		"an empty registry cannot tokenize the id, but the user still typed it")
	require.Equal(t, SelectedInstanceSourceUser, state.SelectedInstanceSource)

	rehydrate(t, eng)
	_, err = eng.ChatWithOptions(context.Background(), "继续排查", noopStep, opts)
	require.NoError(t, err)
	require.Equal(t, 2, cards, "the second turn must reach a real card, not the refusal")
	require.Equal(t, 1, runner.calls)
	require.Equal(t, instanceID, runner.lastReq.InstanceID)
}

// The #546 boundary on the cold path: the model may not manufacture a designation.
// An id the user never wrote is refused at the gate AND leaves the session with no
// selection, so a following bare 「继续排查」 has nothing to inherit either.
func TestAnIDTheUserNeverWroteIsNotADesignationOnAColdRegistry(t *testing.T) {
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("d1", "DiagnoseInstanceInternals",
			`{"UHostId":"cpod-from-a-list","Task":"排查 ComfyUI 打不开"}`)}},
		{Content: "请告诉我要排查哪台实例。"},
	}}
	eng := NewWithDeps(model, &mockExecutor{results: map[string]map[string]any{}}, nil)
	eng.SetInstanceOps(&fakeInstanceOpsRunner{verdict: InstanceOpsVerdict{Text: "不应到达", Ran: 1}})
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)

	cards := 0
	opts := ChatOptions{ConfirmResultFunc: func(string, map[string]any) ConfirmationResult {
		cards++
		return ConfirmationResult{Confirmed: true}
	}}
	_, err := eng.ChatWithOptions(context.Background(), "ComfyUI 打不开", noopStep, opts)
	require.NoError(t, err)
	require.Zero(t, cards, "an id the user never wrote must not reach an authorization card")

	state, _, _ := eng.SessionStateSnapshot()
	require.Empty(t, state.SelectedInstanceID, "and must not become a designation either")
	require.Empty(t, state.SelectedInstanceSource)
}

// The account's sole instance may complete a bare command, but that is the
// ACCOUNT's fact, not something the user pointed at. Recording it as user_selected
// would make "I happen to own one box" indistinguishable from "I named this box",
// and the lane's whole target rule is built on that distinction. This is the
// control that keeps the new record site honest: it fires on the user's literal
// words, not on every id that reaches a card.
func TestTheSoleInstanceReachingACardIsStillNotADesignation(t *testing.T) {
	runner := &fakeInstanceOpsRunner{verdict: InstanceOpsVerdict{Text: "排查完成", Ran: 1}}
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("d1", "DiagnoseInstanceInternals",
			`{"UHostId":"uhost-only-1","Task":"排查 ComfyUI 打不开"}`)}},
		{Content: "排查完成。"},
	}}
	eng := NewWithDeps(model, &mockExecutor{results: map[string]map[string]any{}}, nil)
	eng.SetInstanceOps(runner)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"TotalCount": float64(1), "UHostSet": []any{
			map[string]any{"UHostId": "uhost-only-1", "Name": "solo", "State": "Running"},
		},
	}, "test"))

	opts := ChatOptions{ConfirmResultFunc: func(string, map[string]any) ConfirmationResult {
		return ConfirmationResult{Confirmed: false, TerminalReason: observability.ConfirmationReasonTimeout}
	}}
	_, err := eng.ChatWithOptions(context.Background(), "ComfyUI 打不开", noopStep, opts)
	require.NoError(t, err)

	state, _, _ := eng.SessionStateSnapshot()
	require.NotEqual(t, SelectedInstanceSourceUser, state.SelectedInstanceSource,
		"owning exactly one instance is not the user designating it")
}
