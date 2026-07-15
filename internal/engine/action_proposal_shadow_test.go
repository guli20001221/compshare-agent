package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/actionresolver"
	"github.com/compshare-agent/internal/tools"
	"github.com/stretchr/testify/require"
)

func TestProposeActionShadowResolvesButCannotExecute(t *testing.T) {
	executor := &mockExecutor{}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.lastUserMsg = "停止 uhost-1"
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{"TotalCount": float64(1), "UHostSet": []any{map[string]any{"UHostId": "uhost-1", "Name": "train-a"}}}, "test"))
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-1", time.Now())
	eng.turnContextViewReady = true

	out := eng.executeTool(context.Background(), toolCall("proposal", tools.ProposeActionName,
		`{"turn_id":"turn-1","operation":"StopInstanceWorkflow","slots":[{"name":"UHostId","value":"uhost-1","source":"user_explicit","evidence":{"message_id":"turn-1","start":3,"end":10,"quote":"uhost-1"}}]}`), noopStep)
	var resolved actionresolver.ResolvedAction
	require.NoError(t, json.Unmarshal([]byte(out), &resolved))
	require.True(t, resolved.ReadyForConfirmation)
	require.True(t, resolved.NeedsConfirm)
	require.Empty(t, executor.calls, "shadow proposal must not own an upstream executor")
}

func TestProposeActionShadowRejectsSubstringTarget(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.lastUserMsg = "pytest"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-2", time.Now())
	eng.turnContextViewReady = true
	out := eng.executeTool(context.Background(), toolCall("proposal", tools.ProposeActionName,
		`{"turn_id":"turn-2","operation":"StopInstanceWorkflow","slots":[{"name":"UHostId","value":"test","source":"user_explicit","evidence":{"message_id":"turn-2","start":2,"end":6,"quote":"test"}}]}`), noopStep)
	var resolved actionresolver.ResolvedAction
	require.NoError(t, json.Unmarshal([]byte(out), &resolved))
	require.False(t, resolved.ReadyForConfirmation)
	require.NotEmpty(t, resolved.Rejected)
}

func TestLegacyWorkflowShadowComparisonHasNoExecutor(t *testing.T) {
	executor := &mockExecutor{}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	var events []StepEvent
	eng.observeLegacyWorkflowArguments("StopInstanceWorkflow", map[string]any{"UHostId": "uhost-1"}, func(event StepEvent) { events = append(events, event) })
	require.Empty(t, executor.calls)
	require.Len(t, events, 1)
	require.Equal(t, tools.ProposeActionName, events[0].Action)
	require.Equal(t, "shadow_only", events[0].Source)
}

func TestProposeActionCapabilityRemainsShadowed(t *testing.T) {
	capability, ok := tools.DefaultCapabilityRegistry().Lookup(tools.ProposeActionName)
	require.True(t, ok)
	require.Equal(t, tools.CapabilityStageShadow, capability.Stage)
	for _, tool := range tools.DefaultCapabilityRegistry().VisibleTools(tools.ToolScope{Mode: tools.ToolScopeMutableFull}, true) {
		require.NotEqual(t, tools.ProposeActionName, tool.Function.Name)
	}
}

func TestProposeActionShadowNeverEchoesSensitiveValues(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{"TotalCount": float64(1), "UHostSet": []any{map[string]any{"UHostId": "uhost-1"}}}, "test"))
	eng.lastUserMsg = "重置密码"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-secret", time.Now())
	eng.turnContextViewReady = true
	var events []StepEvent
	out := eng.executeTool(context.Background(), toolCall("proposal", tools.ProposeActionName,
		`{"turn_id":"turn-secret","operation":"ResetPasswordWorkflow","slots":[{"name":"UHostId","value":"uhost-1","source":"verified_context","evidence":{"context_field":"selected_entities"}},{"name":"Password","value":"SecurePass123!","source":"agent_inference"}]}`), func(event StepEvent) { events = append(events, event) })
	require.NotContains(t, out, "SecurePass123!")
	require.Contains(t, out, "[REDACTED]")
	for _, event := range events {
		payload, _ := json.Marshal(event.TraceResult)
		require.False(t, strings.Contains(string(payload), "SecurePass123!"))
	}
}
