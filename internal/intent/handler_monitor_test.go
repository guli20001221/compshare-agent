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
	require.NotNil(t, result.Envelope)
	require.Len(t, result.Envelope.Subjects, 1)
	assert.Equal(t, "uhost-a", result.Envelope.Subjects[0].ID)
	assert.Equal(t, "train-a", result.Envelope.Subjects[0].Name)
	require.Len(t, result.RendererInputEnvelopeHashes, 1)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, result.RendererInputEnvelopeHashes[0])
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
	for _, window := range []*TimeWindow{
		{Type: TimeWindowRelative, Value: "yesterday"},
		{Type: TimeWindowPreset, Value: "today"},
		{Type: TimeWindowAbsolute, Value: "2026-05-08T01:00:00+08:00/2026-05-08T02:00:00+08:00"},
	} {
		t.Run(string(window.Type), func(t *testing.T) {
			exec := &mockHandlerExecutor{}
			handler := NewDemoHandler(exec)

			result := handler.HandleMonitorQuery(context.Background(), HandlerRequest{
				Plan: monitorQueryPlan([]TargetRef{{
					Type:       TargetRefName,
					Value:      "train-a",
					Source:     SourceUserText,
					SourceSpan: "train-a",
				}}, nil, window),
				Resolver: resourceTestSnapshot(t),
			})

			assert.Equal(t, HandlerStatusFallbackBeforeTool, result.Status)
			assert.Equal(t, FallbackTimeWindow, result.FallbackReason)
			assert.Equal(t, CutoverStatusFallbackTimeWindow, result.CutoverStatus)
			assert.Empty(t, exec.calls)
		})
	}
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

func TestMonitorSummaryRendererExtractsSemanticAPIShape(t *testing.T) {
	summary := RenderMonitorSummary([]Metric{MetricCPU, MetricGPU}, monitorAPIResult())

	assert.Contains(t, summary, "CPU 使用率=12.5%")
	assert.Contains(t, summary, "GPU 使用率=87%")
	assert.NotContains(t, summary, "gpu_bus_id")
	assert.NotContains(t, summary, "00:03.0")
	assert.NotContains(t, summary, "系统盘")
	assert.NotContains(t, summary, "数据盘")
	assert.NotContains(t, summary, "显存")
	assert.NotContains(t, summary, "内存")
}

func TestMonitorSummaryRendererReportsMissingRequestedVRAM(t *testing.T) {
	summary := RenderMonitorSummary([]Metric{MetricCPU, MetricVRAM}, map[string]any{
		"Data": map[string]any{
			"List": []any{
				map[string]any{
					"UHostId": "uhost-a",
					"Metrics": []any{
						monitorMetric("uhost_cpu_used", nil, 8),
						monitorMetric("cloudwatch_gpu_memory_usage", map[string]any{"gpu_bus_id": "00:03.0"}),
					},
				},
			},
		},
	})

	assert.Contains(t, summary, "CPU 使用率=8%")
	assert.Contains(t, summary, "显存使用率未返回数据")
}

func TestMonitorSummaryRendererRecognizedAPIShapeWithEmptyValuesDoesNotLeakMetadata(t *testing.T) {
	summary := RenderMonitorSummary([]Metric{MetricGPU}, map[string]any{
		"Data": map[string]any{
			"List": []any{
				map[string]any{
					"UHostId": "uhost-a",
					"Metrics": []any{
						map[string]any{
							"MetricKey": "cloudwatch_gpu_util",
							"Results": []any{
								map[string]any{
									"TagMap": map[string]any{"gpu_bus_id": "00:03.0"},
									"Values": []any{},
								},
							},
						},
					},
				},
			},
		},
	})

	assert.Equal(t, noMonitorValuesReply, summary)
	assert.NotContains(t, summary, "gpu_bus_id")
	assert.NotContains(t, summary, "00:03.0")
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
