package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

// twoInstanceRegistry gives the engine a fresh, complete registry holding two
// running instances. Two, not one, so account-single can never supply the proof
// and every assertion below is about the user's own designation.
func twoInstanceRegistry(t *testing.T, eng *Engine) {
	t.Helper()
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"TotalCount": float64(2), "UHostSet": []any{
			map[string]any{"UHostId": "cpod-typed-1", "Name": "zonereach-wlcb-03", "State": "Running"},
			map[string]any{"UHostId": "uhost-other-2", "Name": "trainer-b", "State": "Running"},
		},
	}, "test"))
}

// rehydrate round-trips the session state through the persisted JSON envelope the
// HTTP lease rebuilds on every request, so these tests prove the PERSISTED
// designation rather than an accidental in-memory carry.
func rehydrate(t *testing.T, eng *Engine) {
	t.Helper()
	state, version, hydrated := eng.SessionStateSnapshot()
	require.True(t, hydrated)
	raw, err := json.Marshal(PersistedContext{AgentSessionState: state})
	require.NoError(t, err)
	persisted, err := ParsePersistedContext(raw)
	require.NoError(t, err)
	eng.ClearSessionState()
	eng.SetSessionState(persisted.AgentSessionState, version+1)
}

// The model-selected run target becomes observed context for later dialogue,
// never a newly minted user-selected proof for platform workflows.
func TestExecutedInstanceIsObservedContextAcrossTurns(t *testing.T) {
	const instanceID = "cpod-typed-1"
	runner := &fakeInstanceOpsRunner{verdict: InstanceOpsVerdict{Text: "排查完成", Ran: 1}}
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("d1", "DiagnoseInstanceInternals",
			`{"UHostId":"cpod-typed-1","Task":"排查 ComfyUI 打不开","Mode":"repair"}`)}},
		{ToolCalls: []openai.ToolCall{toolCall("d2", "DiagnoseInstanceInternals",
			`{"UHostId":"cpod-typed-1","Task":"继续排查 ComfyUI","Mode":"repair"}`)}},
	}}
	eng := NewWithDeps(model, &mockExecutor{results: map[string]map[string]any{}}, nil)
	eng.SetInstanceOps(runner)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	twoInstanceRegistry(t, eng)

	_, err := eng.ChatWithOptions(context.Background(),
		"排查 cpod-typed-1 上的 ComfyUI", noopStep, ChatOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, runner.calls)

	state, _, _ := eng.SessionStateSnapshot()
	require.Equal(t, instanceID, state.SelectedInstanceID)
	require.Equal(t, SelectedInstanceSourceObserved, state.SelectedInstanceSource)

	rehydrate(t, eng)
	_, err = eng.ChatWithOptions(context.Background(), "继续排查", noopStep, ChatOptions{})
	require.NoError(t, err)
	require.Equal(t, 2, runner.calls)
	require.Equal(t, instanceID, runner.lastReq.InstanceID)
}
