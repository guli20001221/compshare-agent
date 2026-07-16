package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/actionresolver"
	"github.com/compshare-agent/internal/capability"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/tools"
	"github.com/stretchr/testify/require"
)

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
	for _, event := range events {
		payload, _ := json.Marshal(event.TraceResult)
		require.False(t, strings.Contains(string(payload), "SecurePass123!"))
	}
}

func TestCentralAgentProposalExecutesOnlyThroughExistingWorkflowGate(t *testing.T) {
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{map[string]any{"UHostId": "uhost-1", "Name": "train-a", "State": "Running", "Zone": "cn-wlcb-01"}}}, nil
		default:
			return map[string]any{"RetCode": 0}, nil
		}
	}}
	confirmCalls := 0
	eng := NewWithDeps(&mockLLM{}, executor, func(action string, args map[string]any) bool {
		confirmCalls++
		require.Equal(t, "StopInstanceWorkflow", action)
		return true
	})
	eng.SetMutatingToolsEnabled(true)
	eng.lastUserMsg = "停止 uhost-1"
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{"TotalCount": float64(1), "UHostSet": []any{map[string]any{"UHostId": "uhost-1", "Name": "train-a", "State": "Running"}}}, "test"))
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-write", time.Now())
	eng.turnContextViewReady = true

	out := eng.executeTool(context.Background(), toolCall("proposal", tools.ProposeActionName,
		`{"turn_id":"turn-write","operation":"StopInstanceWorkflow","slots":[{"name":"UHostId","value":"uhost-1","source":"user_explicit","evidence":{"message_id":"turn-write","start":3,"end":10,"quote":"uhost-1"}}]}`), noopStep)

	require.Contains(t, out, "执行关机")
	require.Equal(t, 1, confirmCalls)
	require.Contains(t, executor.calls, "StopCompShareInstance")
}

func TestCentralAgentProposalCannotExecuteWithoutVerifiedSource(t *testing.T) {
	executor := &mockExecutor{}
	eng := NewWithDeps(&mockLLM{}, executor, func(string, map[string]any) bool {
		t.Fatal("unverified proposal must not reach confirmation")
		return true
	})
	eng.SetMutatingToolsEnabled(true)
	eng.lastUserMsg = "关机"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-unverified", time.Now())
	eng.turnContextViewReady = true

	out := eng.executeTool(context.Background(), toolCall("proposal", tools.ProposeActionName,
		`{"turn_id":"turn-unverified","operation":"StopInstanceWorkflow","slots":[{"name":"UHostId","value":"uhost-invented","source":"agent_inference"}]}`), noopStep)

	require.Contains(t, out, "not verified")
	require.Empty(t, executor.calls)
}

func TestCentralAgentProposalCannotWriteWithoutRequiredJournal(t *testing.T) {
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		if action == "DescribeCompShareInstance" {
			return map[string]any{"UHostSet": []any{map[string]any{"UHostId": "uhost-1", "Name": "train-a", "State": "Running", "Zone": "cn-wlcb-01"}}}, nil
		}
		return map[string]any{"RetCode": 0}, nil
	}}
	confirm := func(string, map[string]any) bool { return true }
	eng := NewWithDeps(&mockLLM{}, executor, confirm)
	eng.safeExecutor = newSafeToolExecutor(executor, confirm, nil, true)
	eng.safeExecutor.SetMutatingToolsEnabled(true)
	eng.SetMutatingToolsEnabled(true)
	eng.lastUserMsg = "停止 uhost-1"
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{"TotalCount": float64(1), "UHostSet": []any{map[string]any{"UHostId": "uhost-1", "Name": "train-a", "State": "Running"}}}, "test"))
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-no-journal", time.Now())
	eng.turnContextViewReady = true

	_ = eng.executeTool(context.Background(), toolCall("proposal", tools.ProposeActionName,
		`{"turn_id":"turn-no-journal","operation":"StopInstanceWorkflow","slots":[{"name":"UHostId","value":"uhost-1","source":"user_explicit","evidence":{"message_id":"turn-no-journal","start":3,"end":10,"quote":"uhost-1"}}]}`), noopStep)

	require.NotContains(t, executor.calls, "StopCompShareInstance")
}

func TestCurrentTurnReadBecomesProposalEvidenceOnlyAfterItWasObserved(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.lastUserMsg = "停止它"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-read-source", time.Now())
	eng.turnContextViewReady = true
	args := map[string]any{
		"turn_id": "turn-read-source", "operation": "StopInstanceWorkflow",
		"slots": []any{map[string]any{"name": "UHostId", "value": "uhost-1", "source": "tool_observation", "evidence": map[string]any{"context_field": "current_turn_read"}}},
	}

	before, err := eng.resolveActionProposalShadow(args)
	require.NoError(t, err)
	require.False(t, before.ReadyForConfirmation)
	eng.readCapabilitySubjectsThisTurn = map[string]struct{}{"uhost-1": {}}
	after, err := eng.resolveActionProposalShadow(args)
	require.NoError(t, err)
	require.True(t, after.ReadyForConfirmation)
}

func TestCurrentTurnCapacityQuoteIsVerifiedAndConvertedBySharedCodec(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.lastUserMsg = "给 uhost-1 加200G数据盘"
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{"TotalCount": float64(1), "UHostSet": []any{map[string]any{"UHostId": "uhost-1"}}}, "test"))
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-capacity", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(map[string]any{
		"operation": "CreateDiskWorkflow",
		"slots": []any{
			map[string]any{"name": "UHostId", "value": "uhost-1", "source": "user_explicit", "evidence": map[string]any{"quote": "uhost-1"}},
			map[string]any{"name": "Size", "value": "200G", "source": "user_explicit", "evidence": map[string]any{"quote": "200G"}},
		},
	})
	require.NoError(t, err)
	require.True(t, resolved.ReadyForConfirmation, resolved.Rejected)
	require.Equal(t, float64(200), resolved.Arguments["Size"])
}

func TestProposalRejectsDifferentTurnEvidence(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, "停止 uhost-1", "active-turn", time.Now())
	eng.turnContextViewReady = true
	_, err := eng.resolveActionProposalShadow(map[string]any{"turn_id": "old-turn", "operation": "StopInstanceWorkflow", "slots": []any{}})
	require.ErrorContains(t, err, "does not match")
}

func TestCentralAgentProposalSchemaComesFromWorkflowCatalog(t *testing.T) {
	window := centralAgentToolWindow(true)
	var stopTool, cfsTool map[string]any
	for _, tool := range window {
		if tool.Function == nil {
			continue
		}
		switch tool.Function.Name {
		case proposalToolName("StopInstanceWorkflow"):
			stopTool, _ = tool.Function.Parameters.(map[string]any)
		case proposalToolName("CreateCFSWorkflow"):
			cfsTool, _ = tool.Function.Parameters.(map[string]any)
		}
	}
	require.NotNil(t, stopTool)
	require.NotNil(t, cfsTool)
	properties := stopTool["properties"].(map[string]any)
	require.NotContains(t, properties, "operation", "the selected proposal tool fixes the operation server-side")
	slots := properties["slots"].(map[string]any)
	items := slots["items"].(map[string]any)
	fields := items["properties"].(map[string]any)["name"].(map[string]any)["enum"].([]string)
	require.Contains(t, fields, "UHostId")
	require.NotContains(t, fields, "Size")
	cfsFields := cfsTool["properties"].(map[string]any)["slots"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)["name"].(map[string]any)["enum"].([]string)
	require.Contains(t, cfsFields, "Size")
}

func TestCentralAgentReadSchemaComesFromCapabilityRegistry(t *testing.T) {
	window := centralAgentToolWindow(false)
	var priceTool, imageTool map[string]any
	for _, tool := range window {
		if tool.Function == nil {
			continue
		}
		switch tool.Function.Name {
		case capability.ReadToolName(intent.IntentPricingQuery):
			priceTool, _ = tool.Function.Parameters.(map[string]any)
		case capability.ReadToolName(intent.IntentImageList):
			imageTool, _ = tool.Function.Parameters.(map[string]any)
		}
	}
	require.NotNil(t, priceTool)
	require.NotNil(t, imageTool)
	priceFields := priceTool["properties"].(map[string]any)
	require.Contains(t, priceFields, "gpu_type")
	require.Contains(t, priceFields, "price_kind")
	require.Contains(t, priceFields, "gpu_count")
	require.NotContains(t, priceFields, "source")
	require.NotContains(t, priceFields, "slots")
	imageFields := imageTool["properties"].(map[string]any)
	require.Contains(t, imageFields, "source")
	require.NotContains(t, imageFields, "price_kind")
	require.NotContains(t, imageFields, "slots")
}

func TestSealedPasswordIsInjectedWithoutEnteringModelArguments(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.secretInputsThisTurn = map[string]string{"Password": "SecurePass123!"}
	eng.readCapabilitySubjectsThisTurn = map[string]struct{}{"uhost-1": {}}
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, "重置密码为[已脱敏:凭据]", "turn-secret", time.Now())
	eng.turnContextViewReady = true
	resolved, err := eng.resolveActionProposalShadow(map[string]any{
		"turn_id": "turn-secret", "operation": "ResetPasswordWorkflow",
		"slots": []any{map[string]any{"name": "UHostId", "value": "uhost-1", "source": "tool_observation", "evidence": map[string]any{"context_field": "current_turn_read"}}},
	})
	require.NoError(t, err)
	require.Equal(t, "SecurePass123!", resolved.Arguments["Password"])
	require.Equal(t, "[REDACTED]", resolved.Confirmation.Arguments["Password"])
}
