package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

// The follow-on shape of TestATypedIDIsADesignationEvenWhenTheRegistryIsEmpty:
// the designation that test records is exactly what broke the NEXT one.
//
// Once cpod-A is carried as a user selection, a turn whose whole message is
// cpod-B used to bind to A. Tier A could not see B — a cold registry derives its
// id prefixes from the instances it holds, and it holds none — so the explicit
// reference that should out-rank carried context was invisible, Tier B returned
// A, and the SSH target gate compared A against B and refused. Production, two
// cPods in one session on 2026-08-23: the second id was typed twice, the second
// time in the exact sentence the assistant had asked for, and refused both times
// with 「只有用户在消息中直接给出的实例 ID」 — which is what the user had just done.
//
// The carried id is what makes the second one legible: it is itself an instance
// id, so its prefix is proven real even when the registry knows nothing.
func TestASecondTypedIDOutranksTheFirstOneItAlreadyBound(t *testing.T) {
	const first, second = "cpod-aaaa1111aaaa", "cpod-bbbb2222bbbb"
	runner := &fakeInstanceOpsRunner{verdict: InstanceOpsVerdict{Text: "排查完成", Ran: 1}}
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("d1", "DiagnoseInstanceInternals",
			`{"UHostId":"cpod-aaaa1111aaaa","Task":"排查 ComfyUI 打不开"}`)}},
		{ToolCalls: []openai.ToolCall{toolCall("d2", "DiagnoseInstanceInternals",
			`{"UHostId":"cpod-bbbb2222bbbb","Task":"排查这台"}`)}},
	}}
	eng := NewWithDeps(model, &mockExecutor{results: map[string]map[string]any{}}, nil)
	eng.SetInstanceOps(runner)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)

	var carded []string
	opts := ChatOptions{ConfirmResultFunc: func(_ string, args map[string]any) ConfirmationResult {
		id, _ := args["UHostId"].(string)
		carded = append(carded, id)
		return ConfirmationResult{Confirmed: true}
	}}

	_, err := eng.ChatWithOptions(context.Background(), "排查 cpod-aaaa1111aaaa 上的 ComfyUI", noopStep, opts)
	require.NoError(t, err)
	require.Equal(t, []string{first}, carded)
	require.Equal(t, first, runner.lastReq.InstanceID)

	state, _, _ := eng.SessionStateSnapshot()
	require.Equal(t, first, state.SelectedInstanceID, "the premise: the first id is now carried context")
	require.Equal(t, SelectedInstanceSourceUser, state.SelectedInstanceSource)

	rehydrate(t, eng)
	_, err = eng.ChatWithOptions(context.Background(), second, noopStep, opts)
	require.NoError(t, err)
	require.Equal(t, []string{first, second}, carded,
		"the id the user typed THIS turn must reach its own card, not the one they typed before")
	require.Equal(t, second, runner.lastReq.InstanceID)
	require.Equal(t, 2, runner.calls)

	state, _, _ = eng.SessionStateSnapshot()
	require.Equal(t, second, state.SelectedInstanceID, "and it becomes the selection going forward")
}

// The #546 boundary has to survive the wider prefix vocabulary. Recognising that a
// message CONTAINS an id-shaped token only suppresses carried context; it grants
// nothing. An id the model produced on its own is still refused — including in the
// session where the wider vocabulary now applies, and even though the user did
// name a real instance of the same family in the same message.
func TestAModelInventedIDIsStillRefusedOnceTheVocabularyIsWider(t *testing.T) {
	const typed, invented = "cpod-aaaa1111aaaa", "cpod-9999invented9"
	runner := &fakeInstanceOpsRunner{verdict: InstanceOpsVerdict{Text: "不应到达", Ran: 1}}
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("d1", "DiagnoseInstanceInternals",
			`{"UHostId":"cpod-aaaa1111aaaa","Task":"排查 ComfyUI"}`)}},
		{ToolCalls: []openai.ToolCall{toolCall("d2", "DiagnoseInstanceInternals",
			`{"UHostId":"cpod-9999invented9","Task":"排查这台"}`)}},
		{Content: "请确认要排查哪台实例。"},
	}}
	eng := NewWithDeps(model, &mockExecutor{results: map[string]map[string]any{}}, nil)
	eng.SetInstanceOps(runner)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)

	var carded []string
	opts := ChatOptions{ConfirmResultFunc: func(_ string, args map[string]any) ConfirmationResult {
		id, _ := args["UHostId"].(string)
		carded = append(carded, id)
		return ConfirmationResult{Confirmed: true}
	}}
	_, err := eng.ChatWithOptions(context.Background(), "排查 cpod-aaaa1111aaaa 上的 ComfyUI", noopStep, opts)
	require.NoError(t, err)
	require.Equal(t, []string{typed}, carded)

	rehydrate(t, eng)
	_, err = eng.ChatWithOptions(context.Background(), "再看看 cpod-aaaa1111aaaa 的显存", noopStep, opts)
	require.NoError(t, err)
	require.Equal(t, []string{typed}, carded,
		"an id the user never wrote must not reach a card just because the message names another id")
	require.Equal(t, typed, runner.lastReq.InstanceID, "and must not be the instance entered")

	state, _, _ := eng.SessionStateSnapshot()
	require.Equal(t, typed, state.SelectedInstanceID, "nor become the selection")
	require.Equal(t, SelectedInstanceSourceUser, state.SelectedInstanceSource)
	require.NotEqual(t, invented, state.SelectedInstanceID)
}
