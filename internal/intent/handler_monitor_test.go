package intent

import (
	"context"
	"errors"
	"testing"
	"time"

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
	assert.Equal(t, RouteStatusDispatched, result.RouteStatus)
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
	assert.Equal(t, RouteStatusFallbackUnresolvedTarget, result.RouteStatus)
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
			assert.Equal(t, RouteStatusFallbackTimeWindow, result.RouteStatus)
			assert.Empty(t, exec.calls)
		})
	}
}

func TestMonitorQueryHandler_HistoricalAbsoluteWindowCallsMonitorWithRange(t *testing.T) {
	exec := &mockHandlerExecutor{result: monitorAPIResult()}
	handler := NewDemoHandler(exec)

	result := handler.HandleMonitorQuery(context.Background(), HandlerRequest{
		Plan: IntentRoute{
			SchemaVersion: SchemaVersion,
			Intent:        IntentMonitorHistory,
			Slots: Slots{
				TargetRefs: []TargetRef{{
					Type:       TargetRefName,
					Value:      "train-a",
					Source:     SourceUserText,
					SourceSpan: "train-a",
				}},
				Metrics: []Metric{MetricCPU, MetricGPU},
				TimeWindow: &TimeWindow{
					Type:  TimeWindowAbsolute,
					Value: "2026-05-08T01:00:00+08:00/2026-05-08T02:00:00+08:00",
				},
			},
			Retrieval:  Retrieval{Enabled: false},
			Confidence: 0.8,
		},
		Resolver: resourceTestSnapshot(t),
	})

	require.Equal(t, HandlerStatusHandled, result.Status)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, "GetCompShareInstanceMonitor", exec.calls[0].action)
	assert.Equal(t, []string{"uhost-a"}, exec.calls[0].args["UHostIds"])
	assert.Equal(t, int64(1778173200), exec.calls[0].args["StartTime"])
	assert.Equal(t, int64(1778176800), exec.calls[0].args["EndTime"])
	assert.Contains(t, result.Reply, "最新")
	assert.Contains(t, result.Reply, "平均")
	assert.Contains(t, result.Reply, "峰值")
}

func TestMonitorHistoryWindowParserAcceptsChineseRangeExamples(t *testing.T) {
	start, end, ok := resolveMonitorHistoryWindow(&TimeWindow{
		Type:  TimeWindowAbsolute,
		Value: "2026-05-08 01:00 到 02:00",
	})

	require.True(t, ok)
	assert.Equal(t, int64(1778173200), start)
	assert.Equal(t, int64(1778176800), end)
}

func TestMonitorHistoryWindowParserAcceptsYesterdayClockRange(t *testing.T) {
	orig := monitorNowFunc
	loc := monitorHistoryLoc
	monitorNowFunc = func() time.Time {
		return time.Date(2026, 6, 22, 12, 0, 0, 0, loc)
	}
	t.Cleanup(func() { monitorNowFunc = orig })

	start, end, ok := ResolveMonitorHistoryWindowFromUserText("查询 uhost-a 昨天 8 点到 10 点的 CPU 历史监控")

	require.True(t, ok)
	assert.Equal(t, time.Date(2026, 6, 21, 8, 0, 0, 0, loc).Unix(), start)
	assert.Equal(t, time.Date(2026, 6, 21, 10, 0, 0, 0, loc).Unix(), end)
}

func TestMonitorHistoryWindowParserAcceptsRecentHoursFromUserText(t *testing.T) {
	orig := monitorNowFunc
	loc := monitorHistoryLoc
	monitorNowFunc = func() time.Time {
		return time.Date(2026, 6, 22, 12, 0, 0, 0, loc)
	}
	t.Cleanup(func() { monitorNowFunc = orig })

	start, end, ok := ResolveMonitorHistoryWindowFromUserText("查询 uhost-a 过去 3 小时 CPU 历史监控")

	require.True(t, ok)
	assert.Equal(t, time.Date(2026, 6, 22, 9, 0, 0, 0, loc).Unix(), start)
	assert.Equal(t, time.Date(2026, 6, 22, 12, 0, 0, 0, loc).Unix(), end)
}

func TestMonitorHistoryWindowParserAcceptsRecentMinutesFromUserText(t *testing.T) {
	orig := monitorNowFunc
	loc := monitorHistoryLoc
	monitorNowFunc = func() time.Time {
		return time.Date(2026, 6, 22, 12, 0, 0, 0, loc)
	}
	t.Cleanup(func() { monitorNowFunc = orig })

	start, end, ok := ResolveMonitorHistoryWindowFromUserText("查询 uhost-a 最近 30 分钟 CPU 历史监控")

	require.True(t, ok)
	assert.Equal(t, time.Date(2026, 6, 22, 11, 30, 0, 0, loc).Unix(), start)
	assert.Equal(t, time.Date(2026, 6, 22, 12, 0, 0, 0, loc).Unix(), end)
}

func TestMonitorHistoryWindowParserAcceptsTodayWithInstancePrefixAndID(t *testing.T) {
	orig := monitorNowFunc
	loc := monitorHistoryLoc
	monitorNowFunc = func() time.Time {
		return time.Date(2026, 6, 22, 12, 0, 0, 0, loc)
	}
	t.Cleanup(func() { monitorNowFunc = orig })

	start, end, ok := ResolveMonitorHistoryWindowFromUserText("实例: uhost-1ry1rvipr0aa 今天 CPU 历史监控")

	require.True(t, ok)
	assert.Equal(t, time.Date(2026, 6, 22, 0, 0, 0, 0, loc).Unix(), start)
	assert.Equal(t, time.Date(2026, 6, 22, 12, 0, 0, 0, loc).Unix(), end)
}

func TestMonitorHistoryWindowParserRejectsRelativeFalsePositives(t *testing.T) {
	cases := []string{
		"查询 uhost-1mabc 上周 CPU 历史监控",
		"查询 uhost-1habc 上周 CPU 历史监控",
		"查询 uhost-a 过去 3 months CPU 历史监控",
		"查询 uhost-a 3 hours CPU 历史监控",
	}
	for _, text := range cases {
		t.Run(text, func(t *testing.T) {
			_, _, ok := ResolveMonitorHistoryWindowFromUserText(text)
			assert.False(t, ok)
		})
	}
}

func TestMonitorHistoryWindowParserRejectsInvalidYesterdayClockRange(t *testing.T) {
	orig := monitorNowFunc
	loc := monitorHistoryLoc
	monitorNowFunc = func() time.Time {
		return time.Date(2026, 6, 22, 12, 0, 0, 0, loc)
	}
	t.Cleanup(func() { monitorNowFunc = orig })

	_, _, ok := ResolveMonitorHistoryWindowFromUserText("查询 uhost-a 昨天 10 点到 9 点的 CPU 历史监控")

	assert.False(t, ok, "invalid explicit range must not degrade to the whole yesterday")
}

func TestMonitorHistoryWindowParserRejectsUnparsedSpecificClockRanges(t *testing.T) {
	cases := []string{
		"查询 uhost-a 昨天 10 点到今天 9 点 CPU 历史监控",
		"查询 uhost-a 今天下午 3 点到 4 点 CPU 历史监控",
		"查询 uhost-a 今天下午 3 点-4 点 CPU 历史监控",
		"查询 uhost-a 昨天 10:00-今天 9:00 CPU 历史监控",
	}
	for _, text := range cases {
		t.Run(text, func(t *testing.T) {
			_, _, ok := ResolveMonitorHistoryWindowFromUserText(text)
			assert.False(t, ok, "unparsed explicit clock range must not degrade to a whole-day range")
		})
	}
}

func TestMonitorQueryHandler_HistoricalMissingWindowRejectedBeforeTool(t *testing.T) {
	exec := &mockHandlerExecutor{result: monitorAPIResult()}
	handler := NewDemoHandler(exec)

	result := handler.HandleMonitorQuery(context.Background(), HandlerRequest{
		Plan: IntentRoute{
			SchemaVersion: SchemaVersion,
			Intent:        IntentMonitorHistory,
			Slots: Slots{
				TargetRefs: []TargetRef{{
					Type:       TargetRefName,
					Value:      "train-a",
					Source:     SourceUserText,
					SourceSpan: "train-a",
				}},
				Metrics: []Metric{MetricCPU},
			},
			Retrieval:  Retrieval{Enabled: false},
			Confidence: 0.8,
		},
		Resolver: resourceTestSnapshot(t),
	})

	assert.Equal(t, HandlerStatusFallbackBeforeTool, result.Status)
	assert.Equal(t, FallbackTimeWindow, result.FallbackReason)
	assert.Empty(t, exec.calls)
}

func TestMonitorQueryHandler_HistoricalWindowOver24HoursRejectedBeforeTool(t *testing.T) {
	exec := &mockHandlerExecutor{result: monitorAPIResult()}
	handler := NewDemoHandler(exec)

	result := handler.HandleMonitorQuery(context.Background(), HandlerRequest{
		Plan: IntentRoute{
			SchemaVersion: SchemaVersion,
			Intent:        IntentMonitorHistory,
			Slots: Slots{
				TargetRefs: []TargetRef{{
					Type:       TargetRefName,
					Value:      "train-a",
					Source:     SourceUserText,
					SourceSpan: "train-a",
				}},
				Metrics:    []Metric{MetricCPU},
				TimeWindow: &TimeWindow{Type: TimeWindowAbsolute, Value: "2026-05-08 01:00 到 2026-05-09 02:00"},
			},
			Retrieval:  Retrieval{Enabled: false},
			Confidence: 0.8,
		},
		Resolver: resourceTestSnapshot(t),
	})

	assert.Equal(t, HandlerStatusFallbackBeforeTool, result.Status)
	assert.Equal(t, FallbackTimeWindow, result.FallbackReason)
	assert.Empty(t, exec.calls)
}

func TestHistoricalMonitorSummaryExtractsPodListShape(t *testing.T) {
	summary := RenderHistoricalMonitorSummary(nil, map[string]any{
		"Data": map[string]any{
			"PodList": []any{
				map[string]any{
					"UHostId": "cpod-a",
					"Metrics": map[string]any{
						"Cpu":         []any{monitorPoint(10, 0), monitorPoint(20, 1)},
						"Memory":      []any{monitorPoint(30, 0), monitorPoint(40, 1)},
						"SysDiskUsed": []any{monitorPoint(50, 0), monitorPoint(60, 1)},
						"Gpu": []any{
							map[string]any{
								"GpuIndex": "0",
								"Util":     []any{monitorPoint(70, 0), monitorPoint(80, 1)},
								"Memory":   []any{monitorPoint(90, 0), monitorPoint(95, 1)},
							},
						},
					},
				},
			},
		},
	})

	assert.Contains(t, summary, "CPU 使用率")
	assert.Contains(t, summary, "内存使用率")
	assert.Contains(t, summary, "系统盘使用率")
	assert.Contains(t, summary, "GPU 使用率")
	assert.Contains(t, summary, "显存使用率")
	assert.NotContains(t, summary, "SysDiskUsed")
	assert.NotContains(t, summary, "GpuIndex")
}

func TestMonitorQueryHandler_HistoricalMultipleTargetsRejectedBeforeTool(t *testing.T) {
	exec := &mockHandlerExecutor{result: monitorAPIResult()}
	handler := NewDemoHandler(exec)

	result := handler.HandleMonitorQuery(context.Background(), HandlerRequest{
		Plan: IntentRoute{
			SchemaVersion: SchemaVersion,
			Intent:        IntentMonitorHistory,
			Slots: Slots{
				TargetRefs: []TargetRef{
					{Type: TargetRefName, Value: "train-a", Source: SourceUserText, SourceSpan: "train-a"},
					{Type: TargetRefName, Value: "train-b", Source: SourceUserText, SourceSpan: "train-b"},
				},
				TimeWindow: &TimeWindow{Type: TimeWindowPreset, Value: "yesterday"},
			},
			Retrieval:  Retrieval{Enabled: false},
			Confidence: 0.8,
		},
		Resolver: resourceTestSnapshot(t),
	})

	assert.Equal(t, HandlerStatusFallbackBeforeTool, result.Status)
	assert.Equal(t, FallbackValidation, result.FallbackReason)
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
	assert.Equal(t, RouteStatusFailureAfterTool, result.RouteStatus)
	assert.Contains(t, result.Reply, FriendlyToolFailureReply)
	assert.NotContains(t, result.Reply, "raw monitor provider error")
	assert.Equal(t, "GetCompShareInstanceMonitor", result.ToolAction)
	assert.Equal(t, []string{"uhost-a"}, result.ToolArgs["UHostIds"])
}

func TestMonitorQueryHandler_FallbackInstanceIDUsedWhenTargetRefsEmpty(t *testing.T) {
	exec := &mockHandlerExecutor{result: monitorResult()}
	handler := NewDemoHandler(exec)

	result := handler.HandleMonitorQuery(context.Background(), HandlerRequest{
		Plan:               monitorQueryPlan(nil, nil, nil),
		Resolver:           resourceTestSnapshot(t),
		FallbackInstanceID: "uhost-a",
	})

	require.Equal(t, HandlerStatusHandled, result.Status)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, "GetCompShareInstanceMonitor", exec.calls[0].action)
	assert.Equal(t, []string{"uhost-a"}, exec.calls[0].args["UHostIds"])
}

func TestMonitorQueryHandler_FallbackInstanceIDIgnoredWhenTargetRefsPresent(t *testing.T) {
	exec := &mockHandlerExecutor{result: monitorResult()}
	handler := NewDemoHandler(exec)

	result := handler.HandleMonitorQuery(context.Background(), HandlerRequest{
		Plan: monitorQueryPlan([]TargetRef{{
			Type:       TargetRefName,
			Value:      "train-a",
			Source:     SourceUserText,
			SourceSpan: "train-a",
		}}, nil, nil),
		Resolver:           resourceTestSnapshot(t),
		FallbackInstanceID: "uhost-should-not-be-used",
	})

	require.Equal(t, HandlerStatusHandled, result.Status)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, []string{"uhost-a"}, exec.calls[0].args["UHostIds"])
}

func TestMonitorQueryHandler_FallbackInstanceIDNotInSnapshotTriggersSelection(t *testing.T) {
	exec := &mockHandlerExecutor{result: monitorResult()}
	handler := NewDemoHandler(exec)

	result := handler.HandleMonitorQuery(context.Background(), HandlerRequest{
		Plan:               monitorQueryPlan(nil, nil, nil),
		Resolver:           resourceTestSnapshot(t),
		FallbackInstanceID: "uhost-deleted-long-ago",
	})

	assert.Equal(t, HandlerStatusFallbackBeforeTool, result.Status)
	assert.Equal(t, FallbackUnresolvedTarget, result.FallbackReason)
	assert.Empty(t, exec.calls)
}

func monitorQueryPlan(refs []TargetRef, metrics []Metric, window *TimeWindow) IntentRoute {
	return IntentRoute{
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
