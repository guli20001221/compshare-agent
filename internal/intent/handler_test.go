package intent

import (
	"strings"
	"testing"

	"github.com/compshare-agent/internal/entity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandlerResultStatesAreDistinct(t *testing.T) {
	handled := HandledResult("ok")
	fallback := FallbackBeforeTool(FallbackUnresolvedTarget)
	failure := FailureAfterTool("monitor")

	assert.Equal(t, HandlerStatusHandled, handled.Status)
	assert.Equal(t, HandlerStatusFallbackBeforeTool, fallback.Status)
	assert.Equal(t, HandlerStatusFailureAfterTool, failure.Status)
	assert.Equal(t, FallbackUnresolvedTarget, fallback.FallbackReason)
	assert.Equal(t, CutoverStatusFailureAfterTool, failure.CutoverStatus)
	assert.Contains(t, failure.Reply, FriendlyToolFailureReply)
	assert.NotContains(t, failure.Reply, assert.AnError.Error())
	assert.Equal(t, CutoverStatusFallbackTimeWindow, FallbackBeforeTool(FallbackTimeWindow).CutoverStatus)
}

func TestResourceSummaryRendererIsDeterministicAndRedactsSensitiveFields(t *testing.T) {
	instances := []entity.InstanceSnapshot{
		{
			UHostId:    "uhost-b",
			Name:       "Authorization: Bearer " + strings.Repeat("b", 25),
			State:      "Stopped",
			GpuType:    "4090",
			GPU:        1,
			CPU:        8,
			Memory:     65536,
			ImageType:  "Ubuntu",
			ChargeType: "Day",
			StartTime:  2000,
			ExpireTime: 3000,
		},
		{
			UHostId:    "uhost-a",
			Name:       "train-a",
			State:      "Running",
			GpuType:    "A100",
			GPU:        2,
			CPU:        16,
			Memory:     131072,
			ImageType:  "CentOS",
			ChargeType: "Dynamic",
			StartTime:  1000,
		},
	}

	first := RenderResourceSummary(instances)
	second := RenderResourceSummary(instances)

	require.Equal(t, first, second)
	assert.Less(t, strings.Index(first, "uhost-a"), strings.Index(first, "uhost-b"), "summary must be sorted by UHostId")
	assert.Contains(t, first, "train-a")
	assert.Contains(t, first, "Running")
	assert.Contains(t, first, "A100")
	assert.Contains(t, first, resourceLabelInstanceID)
	assert.Contains(t, first, resourceLabelName)
	assert.NotContains(t, first, "Name=")
	assert.NotContains(t, first, "State=")
	assert.Contains(t, first, "128 GB")
	assert.Contains(t, first, "64 GB")
	assert.NotContains(t, first, "65536")
	assert.NotContains(t, first, "StartTime")
	assert.NotContains(t, first, "ExpireTime")
	assert.Contains(t, first, resourceLabelChargeType)
	assert.NotContains(t, first, strings.Repeat("b", 25))
	assert.Contains(t, first, "Bearer [REDACTED]")
}

func TestMonitorSummaryRendererEmptyMetricsRendersAvailableCurrentValues(t *testing.T) {
	result := map[string]any{
		"CPU":    float64(12.5),
		"GPU":    float64(87),
		"Memory": float64(64),
		"VRAM":   "20GB",
		"Nested": map[string]any{
			"Authorization": "Bearer " + strings.Repeat("c", 25),
		},
	}

	summary := RenderMonitorSummary(nil, result)

	assert.Contains(t, summary, "CPU")
	assert.Contains(t, summary, "12.5")
	assert.Contains(t, summary, "GPU")
	assert.Contains(t, summary, "87")
	assert.Contains(t, summary, "Memory")
	assert.Contains(t, summary, "VRAM")
	assert.NotContains(t, summary, strings.Repeat("c", 25))
}

func TestMonitorSummaryRendererRendersCompShareMonitorPayloadAsUserFacingValues(t *testing.T) {
	summary := RenderMonitorSummary(nil, compShareMonitorPayload())

	assert.Contains(t, summary, monitorLabelCPU)
	assert.Contains(t, summary, "12.5%")
	assert.Contains(t, summary, monitorLabelMemory)
	assert.Contains(t, summary, "4%")
	assert.Contains(t, summary, monitorLabelGPU)
	assert.Contains(t, summary, "87%")
	assert.Contains(t, summary, monitorLabelVRAM)
	assert.Contains(t, summary, "20%")
	assert.NotContains(t, summary, "Data.List")
	assert.NotContains(t, summary, "MetricKey")
	assert.NotContains(t, summary, "TagMap")
	assert.NotContains(t, summary, "gpu_bus_id")
	assert.NotContains(t, summary, "Results[")
}

func TestMonitorSummaryRendererFiltersRequestedCompShareMetrics(t *testing.T) {
	summary := RenderMonitorSummary([]Metric{MetricGPU, MetricVRAM}, compShareMonitorPayload())

	assert.NotContains(t, summary, monitorLabelCPU)
	assert.NotContains(t, summary, monitorLabelMemory)
	assert.Contains(t, summary, monitorLabelGPU)
	assert.Contains(t, summary, monitorLabelVRAM)
	assert.NotContains(t, summary, "gpu_bus_id")
}

func TestMonitorSummaryRendererDropsUnknownCompShareMetricKeys(t *testing.T) {
	payload := map[string]any{
		"Data": map[string]any{
			"List": []any{
				map[string]any{
					"Metrics": []any{
						monitorMetric("unknown_internal_metric", float64(99), map[string]any{"gpu_bus_id": "00:03.0"}),
					},
				},
			},
		},
	}

	summary := RenderMonitorSummary(nil, payload)

	assert.Equal(t, noMonitorValuesReply, summary)
	assert.NotContains(t, summary, "unknown_internal_metric")
	assert.NotContains(t, summary, "gpu_bus_id")
}

func TestMonitorSummaryRendererFiltersRequestedMetrics(t *testing.T) {
	result := map[string]any{
		"CPU":    float64(12.5),
		"GPU":    float64(87),
		"Memory": float64(64),
		"VRAM":   "20GB",
	}

	summary := RenderMonitorSummary([]Metric{MetricGPU, MetricVRAM}, result)

	assert.NotContains(t, summary, "CPU")
	assert.NotContains(t, summary, "Memory")
	assert.Contains(t, summary, "GPU")
	assert.Contains(t, summary, "VRAM")
}

func compShareMonitorPayload() map[string]any {
	return map[string]any{
		"Data": map[string]any{
			"List": []any{
				map[string]any{
					"UHostId": "uhost-a",
					"Metrics": []any{
						monitorMetric("uhost_cpu_used", float64(12.5), nil),
						monitorMetric("cloudwatch_memory_usage", float64(4), nil),
						monitorMetric("cloudwatch_gpu_util", float64(87), map[string]any{"gpu_bus_id": "00:03.0"}),
						monitorMetric("cloudwatch_gpu_memory_usage", float64(20), map[string]any{"gpu_bus_id": "00:03.0"}),
					},
				},
			},
		},
	}
}

func monitorMetric(metricKey string, value float64, tagMap map[string]any) map[string]any {
	result := map[string]any{
		"Values": []any{
			map[string]any{"Timestamp": float64(1778317200), "Value": value},
		},
	}
	if tagMap != nil {
		result["TagMap"] = tagMap
	}
	return map[string]any{
		"MetricKey": metricKey,
		"Tags":      map[string]any{"gpu_bus_id": []any{"00:03.0"}},
		"Results":   []any{result},
	}
}

func TestHandlerActionWhitelist(t *testing.T) {
	assert.True(t, IsAllowedHandlerAction(IntentResourceInfo, "DescribeCompShareInstance"))
	assert.False(t, IsAllowedHandlerAction(IntentResourceInfo, "GetCompShareInstanceMonitor"))
	assert.True(t, IsAllowedHandlerAction(IntentMonitorQuery, "GetCompShareInstanceMonitor"))
	assert.False(t, IsAllowedHandlerAction(IntentMonitorQuery, "StopCompShareInstance"))
	assert.Nil(t, RequireAllowedHandlerAction(IntentMonitorQuery, "GetCompShareInstanceMonitor"))

	result := RequireAllowedHandlerAction(IntentMonitorQuery, "StopCompShareInstance")
	require.NotNil(t, result)
	assert.Equal(t, HandlerStatusFallbackBeforeTool, result.Status)
	assert.Equal(t, FallbackActionNotAllowed, result.FallbackReason)
	assert.Equal(t, CutoverStatusFallbackIneligible, result.CutoverStatus)
}
