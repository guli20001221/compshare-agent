package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/tools"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

func TestReadPlatformCapabilityIsExecutableButNotAdvertised(t *testing.T) {
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
	eng.lastUserMsg = "列出我的实例"
	var events []StepEvent
	out := eng.executeTool(context.Background(), openai.ToolCall{Function: openai.FunctionCall{
		Name: tools.ReadPlatformCapabilityName, Arguments: `{"capability":"resource_info","slots":{}}`,
	}}, func(event StepEvent) { events = append(events, event) })

	var observation ReadCapabilityObservation
	require.NoError(t, json.Unmarshal([]byte(out), &observation))
	require.Equal(t, intent.HandlerStatusHandled, observation.Status)
	require.NotNil(t, observation.Envelope)
	require.True(t, observation.DirectSubmitEligible)
	require.Equal(t, "DescribeCompShareInstance", observation.ToolAction)
	require.Contains(t, executor.calls, "DescribeCompShareInstance")
	require.NotEmpty(t, events)

	for _, tool := range tools.DefaultCapabilityRegistry().VisibleTools(tools.ToolScope{Mode: tools.ToolScopeReadOnlyFull}, true) {
		require.NotEqual(t, tools.ReadPlatformCapabilityName, tool.Function.Name)
	}
}

func TestReadPlatformCapabilityPreservesFailureAndAbsenceSemantics(t *testing.T) {
	t.Run("incomplete registry cannot assert absence", func(t *testing.T) {
		eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
		eng.lastUserMsg = "查询 uhost-missing"
		out := eng.executeTool(context.Background(), toolCall("read", tools.ReadPlatformCapabilityName,
			`{"capability":"resource_info","slots":{"target_refs":[{"type":"uhost_id_user_input","value":"uhost-missing","source":"user_text","source_span":"uhost-missing"}]}}`), noopStep)
		var observation ReadCapabilityObservation
		require.NoError(t, json.Unmarshal([]byte(out), &observation))
		require.Equal(t, intent.HandlerStatusFallbackBeforeTool, observation.Status)
		require.Equal(t, intent.FallbackUnresolvedTarget, observation.FallbackReason)
		require.False(t, observation.CanAssertAbsence)
		require.False(t, observation.DirectSubmitEligible)
	})

	t.Run("tool failure is not missing input", func(t *testing.T) {
		eng := NewWithDeps(&mockLLM{}, &mockExecutorFn{fn: func(string, map[string]any) (map[string]any, error) {
			return nil, errors.New("upstream unavailable")
		}}, nil)
		eng.lastUserMsg = "列出我的实例"
		out := eng.executeTool(context.Background(), toolCall("read", tools.ReadPlatformCapabilityName,
			`{"capability":"resource_info","slots":{}}`), noopStep)
		var observation ReadCapabilityObservation
		require.NoError(t, json.Unmarshal([]byte(out), &observation))
		require.Equal(t, intent.HandlerStatusFailureAfterTool, observation.Status)
		require.Empty(t, observation.FallbackReason)
		require.False(t, observation.DirectSubmitEligible)
	})
}

func TestReadPlatformCapabilityRejectsWriteAndUnknownSlots(t *testing.T) {
	_, _, err := decodeReadCapabilityArgs(map[string]any{"capability": string(intent.IntentOperationLifecycle)})
	require.ErrorContains(t, err, "not a registered read capability")

	_, _, err = decodeReadCapabilityArgs(map[string]any{
		"capability": string(intent.IntentResourceInfo),
		"slots":      map[string]any{"invented_field": "value"},
	})
	require.ErrorContains(t, err, "unknown field")
}

func TestReadPlatformCapabilityCoversCatalogWithoutIntentList(t *testing.T) {
	capabilities := append([]intent.Intent{intent.IntentResourceInfo, intent.IntentMonitorQuery, intent.IntentMonitorHistory}, intent.RoutingIntents()...)
	for _, capability := range capabilities {
		t.Run(string(capability), func(t *testing.T) {
			got, _, err := decodeReadCapabilityArgs(map[string]any{"capability": string(capability), "slots": map[string]any{}})
			require.NoError(t, err)
			require.Equal(t, capability, got)
		})
	}
}
