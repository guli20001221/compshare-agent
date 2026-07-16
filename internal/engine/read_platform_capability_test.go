package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/compshare-agent/internal/capability"
	"github.com/compshare-agent/internal/intent"
	"github.com/stretchr/testify/require"
)

func TestGenericReadPlatformCapabilityIsRemoved(t *testing.T) {
	_, ok := capability.ReadIntentForTool("ReadPlatformCapability")
	require.False(t, ok)
}

func TestConcreteReadAlwaysReturnsObservationAndNeverEndsTurn(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"TotalCount": float64(1),
			"UHostSet": []any{map[string]any{
				"UHostId": "uhost-1", "Name": "train-a", "State": "Running",
				"GpuType": "4090", "GPU": float64(1), "CPU": float64(8), "Memory": float64(64),
			}},
		},
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	out := eng.executeTool(context.Background(), toolCall("read", capability.ReadToolName(intent.IntentResourceInfo),
		`{}`), noopStep)
	_, ok := isFinalReply(out)
	require.False(t, ok, "a read capability is an observation and must never end the turn")
	var observation ReadCapabilityObservation
	require.NoError(t, json.Unmarshal([]byte(out), &observation))
	require.Equal(t, intent.HandlerStatusHandled, observation.Status)
	require.NotNil(t, observation.Envelope)
}

func TestConcreteReadReturnsStructuredMissingFieldsBeforeHandler(t *testing.T) {
	executor := &mockExecutor{}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	out := eng.executeTool(context.Background(), toolCall("read", capability.ReadToolName(intent.IntentPricingQuery), `{}`), noopStep)

	var observation ReadCapabilityObservation
	require.NoError(t, json.Unmarshal([]byte(out), &observation))
	require.Equal(t, intent.HandlerStatusNeedsInput, observation.Status)
	require.Equal(t, []capability.MissingField{{Name: "gpu_type", Reason: "required"}}, observation.MissingFields)
	require.Empty(t, executor.calls, "缺失字段必须在能力边界返回，不能进入 handler 或上游 API")
}

// TestAccountFinanceUnavailableReturnsStructuredUnavailable: the model-visible
// account-finance tool returns a structured Unavailable observation (status +
// reason + alternatives) without ever calling an upstream API, so a balance /
// invoice question gets a deterministic non-fabricated answer.
func TestAccountFinanceUnavailableReturnsStructuredUnavailable(t *testing.T) {
	executor := &mockExecutor{}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	out := eng.executeTool(context.Background(), toolCall("read", capability.ReadToolName(intent.Intent("account_finance_status")), `{}`), noopStep)

	if _, ok := isFinalReply(out); ok {
		t.Fatal("an unavailable capability is an observation and must never end the turn")
	}
	var observation UnavailableCapabilityObservation
	require.NoError(t, json.Unmarshal([]byte(out), &observation))
	require.Equal(t, "unavailable", observation.Status)
	require.Equal(t, "account_finance_status", observation.Capability)
	require.Contains(t, observation.Reason, "不支持直接查询账号余额")
	require.NotEmpty(t, observation.Alternatives)
	require.Empty(t, executor.calls, "an unavailable capability must not call any upstream API")
}
