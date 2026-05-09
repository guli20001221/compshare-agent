package intent

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMonitorQueryHandler_ValidTargetCallsMonitorAndReturnsTraceMetadata(t *testing.T) {
	resolver := resourceTestSnapshot(t)
	exec := &mockHandlerExecutor{result: monitorResult()}
	handler := NewDemoHandler(exec)

	result := handler.HandleMonitorQuery(context.Background(), HandlerRequest{
		Plan: monitorQueryPlan([]TargetRef{{
			Type:       TargetRefName,
			Value:      "train-a",
			Source:     SourceUserText,
			SourceSpan: "train-a",
		}}, nil, nil),
		Resolver: resolver,
	})

	require.Equal(t, HandlerStatusHandled, result.Status)
	assert.Equal(t, CutoverStatusDispatched, result.CutoverStatus)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, "GetCompShareInstanceMonitor", exec.calls[0].action)
	assert.Equal(t, []string{"uhost-a"}, exec.calls[0].args["UHostIds"])
	assert.Equal(t, "GetCompShareInstanceMonitor", result.ToolAction)
	assert.Equal(t, []string{"uhost-a"}, result.ToolArgs["UHostIds"])
	require.Len(t, result.RendererInputToolArgHashes, 1)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, result.RendererInputToolArgHashes[0])
	assert.Contains(t, result.Reply, "GPU")
	assert.Contains(t, result.Reply, "VRAM")
}

func TestMonitorQueryHandler_MissingTargetFallsBackBeforeTool(t *testing.T) {
	exec := &mockHandlerExecutor{}
	handler := NewDemoHandler(exec)

	result := handler.HandleMonitorQuery(context.Background(), HandlerRequest{
		Plan:     monitorQueryPlan(nil, nil, nil),
		Resolver: resourceTestSnapshot(t),
	})

	assert.Equal(t, HandlerStatusFallbackBeforeTool, result.Status)
	assert.Equal(t, FallbackMissingTarget, result.FallbackReason)
	assert.Equal(t, CutoverStatusFallbackUnresolvedTarget, result.CutoverStatus)
	assert.Empty(t, exec.calls)
}

func TestMonitorQueryHandler_NonCurrentTimeWindowFallsBackBeforeTool(t *testing.T) {
	exec := &mockHandlerExecutor{}
	handler := NewDemoHandler(exec)

	result := handler.HandleMonitorQuery(context.Background(), HandlerRequest{
		Plan: monitorQueryPlan([]TargetRef{{
			Type:       TargetRefName,
			Value:      "train-a",
			Source:     SourceUserText,
			SourceSpan: "train-a",
		}}, nil, &TimeWindow{Type: TimeWindowRelative, Value: "yesterday"}),
		Resolver: resourceTestSnapshot(t),
	})

	assert.Equal(t, HandlerStatusFallbackBeforeTool, result.Status)
	assert.Equal(t, FallbackTimeWindow, result.FallbackReason)
	assert.Equal(t, CutoverStatusFallbackTimeWindow, result.CutoverStatus)
	assert.Empty(t, exec.calls)
}

func TestMonitorQueryHandler_CurrentPresetTimeWindowIsAllowed(t *testing.T) {
	exec := &mockHandlerExecutor{result: monitorResult()}
	handler := NewDemoHandler(exec)

	result := handler.HandleMonitorQuery(context.Background(), HandlerRequest{
		Plan: monitorQueryPlan([]TargetRef{{
			Type:       TargetRefUHostIDUserInput,
			Value:      "uhost-a",
			Source:     SourceUserText,
			SourceSpan: "uhost-a",
		}}, []Metric{MetricGPU}, &TimeWindow{Type: TimeWindowPreset, Value: "now"}),
		Resolver: resourceTestSnapshot(t),
	})

	require.Equal(t, HandlerStatusHandled, result.Status)
	require.Len(t, exec.calls, 1)
	assert.Contains(t, result.Reply, "GPU")
	assert.NotContains(t, result.Reply, "CPU")
}

func TestMonitorQueryHandler_APIFailureReturnsFriendlyFailureWithTraceMetadata(t *testing.T) {
	exec := &mockHandlerExecutor{err: errors.New("raw monitor provider error")}
	handler := NewDemoHandler(exec)

	result := handler.HandleMonitorQuery(context.Background(), HandlerRequest{
		Plan: monitorQueryPlan([]TargetRef{{
			Type:       TargetRefUHostIDUserInput,
			Value:      "uhost-a",
			Source:     SourceUserText,
			SourceSpan: "uhost-a",
		}}, nil, nil),
		Resolver: resourceTestSnapshot(t),
	})

	assert.Equal(t, HandlerStatusFailureAfterTool, result.Status)
	assert.Equal(t, CutoverStatusFailureAfterTool, result.CutoverStatus)
	assert.Contains(t, result.Reply, FriendlyToolFailureReply)
	assert.NotContains(t, result.Reply, "raw monitor provider error")
	assert.Equal(t, "GetCompShareInstanceMonitor", result.ToolAction)
	assert.Equal(t, []string{"uhost-a"}, result.ToolArgs["UHostIds"])
}

func monitorQueryPlan(refs []TargetRef, metrics []Metric, window *TimeWindow) Plan {
	return Plan{
		SchemaVersion: SchemaVersion,
		Intent:        IntentMonitorQuery,
		Slots: Slots{
			TargetRefs: refs,
			Metrics:    metrics,
			TimeWindow: window,
		},
		Retrieval:  Retrieval{Enabled: false},
		Confidence: 0.8,
	}
}

func monitorResult() map[string]any {
	return map[string]any{
		"CPU":  float64(12.5),
		"GPU":  float64(87),
		"VRAM": "20GB",
	}
}

