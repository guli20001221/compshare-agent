package readprojection

import (
	"strings"
	"testing"

	"github.com/compshare-agent/internal/entity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResourceSummaryRendererIsDeterministicAndRedactsSensitiveFields(t *testing.T) {
	instances := []entity.InstanceSnapshot{
		{
			UHostId:    "uhost-b",
			Name:       "Authorization: Bearer " + strings.Repeat("b", 25),
			State:      "Stopped",
			GpuType:    "4090",
			GPU:        1,
			CPU:        8,
			Memory:     64,
			ImageType:  "Ubuntu",
			StartTime:  2000,
			ExpireTime: 3000,
		},
		{
			UHostId:   "uhost-a",
			Name:      "train-a",
			State:     "Running",
			GpuType:   "A100",
			GPU:       2,
			CPU:       16,
			Memory:    128,
			ImageType: "CentOS",
			StartTime: 1000,
		},
	}

	SortInstancesForDisplay(instances)
	first := RenderResourceSummary(instances, ResourceEnvelopeMeta{})
	second := RenderResourceSummary(instances, ResourceEnvelopeMeta{})

	require.Equal(t, first, second)
	assert.Less(t, strings.Index(first, "uhost-a"), strings.Index(first, "uhost-b"), "Running uhost-a should rank above Stopped uhost-b in display order")
	assert.Contains(t, first, "train-a")
	assert.Contains(t, first, "运行中")
	assert.Contains(t, first, "A100")
	assert.Contains(t, first, "uhost-a")
	assert.Contains(t, first, "vCPU")
	assert.NotContains(t, first, "Name=")
	assert.NotContains(t, first, "State=")
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
