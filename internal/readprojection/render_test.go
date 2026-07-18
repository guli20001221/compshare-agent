package readprojection

import (
	"testing"

	"github.com/compshare-agent/internal/entity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRenderResourceSummaryByteExact pins the resource summary's exact output
// for one fully-populated instance — the byte-equivalence anchor for the
// intent→readprojection relocation. The intent wrapper test (handler_resource
// _test.go) asserts the same shape via HandleResourceInfo; this exercises the
// single implementation directly.
func TestRenderResourceSummaryByteExact(t *testing.T) {
	instances := []entity.InstanceSnapshot{{
		UHostId:    "uhost-a",
		Name:       "train-a",
		State:      "Running",
		GpuType:    "4090",
		GPU:        1,
		CPU:        8,
		Memory:     64,
		ImageType:  "Ubuntu",
		StartTime:  1000,
		ExpireTime: 2000,
	}}

	got := RenderResourceSummary(instances, ResourceEnvelopeMeta{})

	want := "实例ID=uhost-a, 名称=train-a, 状态=Running, GPU型号=4090, GPU数量=1, CPU=8, 内存=64, 镜像类型=Ubuntu, 启动时间=1000, 到期时间=2000"
	require.Equal(t, want, got)
}

// TestRenderResourceSummaryTruncationNotice pins the deterministic truncation
// sentence (the answering model must not freelance 分页 wording here).
func TestRenderResourceSummaryTruncationNotice(t *testing.T) {
	instances := []entity.InstanceSnapshot{{UHostId: "uhost-a", Name: "a", State: "Running"}}

	got := RenderResourceSummary(instances, ResourceEnvelopeMeta{TotalCount: 12, Shown: 10, Truncated: true})

	assert.Contains(t, got, "（已显示 10/12 台，完整列表请到控制台查看）")
}

func TestRenderResourceSummaryEmptyIsNoInstances(t *testing.T) {
	assert.Equal(t, "未找到实例。", RenderResourceSummary(nil, ResourceEnvelopeMeta{}))
}

// TestRenderMonitorSummarySemanticFacts pins the current-monitor render on the
// recognized API shape: only the requested metrics, with their canonical
// labels + unit.
func TestRenderMonitorSummarySemanticFacts(t *testing.T) {
	got := RenderMonitorSummary([]Metric{MetricCPU, MetricGPU}, monitorAPIResult())

	assert.Equal(t, "CPU 使用率=12.5%; GPU 使用率=87%", got)
}

// TestRenderMonitorSummaryNotesMissingRequestedMetric pins the "未返回数据"
// suffix for a requested-but-absent metric — the anti-fabrication guarantee.
func TestRenderMonitorSummaryNotesMissingRequestedMetric(t *testing.T) {
	payload := map[string]any{
		"Data": map[string]any{
			"List": []any{
				map[string]any{
					"UHostId": "uhost-a",
					"Metrics": []any{monitorMetric("uhost_cpu_used", nil, 8)},
				},
			},
		},
	}

	got := RenderMonitorSummary([]Metric{MetricCPU, MetricVRAM}, payload)

	assert.Contains(t, got, "CPU 使用率=8%")
	assert.Contains(t, got, "显存使用率未返回数据")
}

func TestRenderMonitorSummaryEmptyIsNoValues(t *testing.T) {
	assert.Equal(t, "未返回监控数据。", RenderMonitorSummary(nil, map[string]any{}))
}

// TestExtractMonitorScalarsSharesRendererVocabulary pins that the engine
// ToolFact writer's scalar keys match the renderer vocabulary (cpu_usage,
// gpu_usage, …) — the single-source guarantee behind re-pointing the writer.
func TestExtractMonitorScalarsSharesRendererVocabulary(t *testing.T) {
	scalars := ExtractMonitorScalars(monitorAPIResult(), nil)

	byKey := map[string]MonitorScalar{}
	for _, s := range scalars {
		byKey[s.Key] = s
	}
	require.Contains(t, byKey, "cpu_usage")
	require.Contains(t, byKey, "gpu_usage")
	assert.Equal(t, "12.5", byKey["cpu_usage"].Value)
	assert.Equal(t, "%", byKey["cpu_usage"].Unit)
	assert.Equal(t, "uhost-a", byKey["cpu_usage"].SubjectID)
}

// TestRenderHistoricalMonitorSummaryStatesWindow pins the structured replacement for
// the deleted engine date-regex: the deterministic historical reply itself states
// the exact queried Beijing window, so the model no longer needs post-hoc correction.
func TestRenderHistoricalMonitorSummaryStatesWindow(t *testing.T) {
	// 1777442400 = 2026-04-29 14:00, 1777444200 = 14:30 (Asia/Shanghai, UTC+8).
	got := RenderHistoricalMonitorSummary(nil, map[string]any{}, 1777442400, 1777444200)
	assert.Contains(t, got, "北京时间 2026-04-29 14:00 ~ 14:30（历史时间窗）", "the reply states the exact queried window")
	assert.Contains(t, got, "未返回监控数据", "empty payload is window-scoped no-data, never a fabricated 0%/healthy value")
}

// An unset window (current monitor / window-less caller) adds no prefix.
func TestRenderHistoricalMonitorSummaryNoWindowNoPrefix(t *testing.T) {
	assert.Equal(t, "未返回监控数据。", RenderHistoricalMonitorSummary(nil, map[string]any{}, 0, 0))
}

// TestBuildHistoricalMonitorEnvelopeMarksWindowRange pins that historical facts carry
// the window as structured evidence (Period=range + WindowStart/End) — the Observation
// half of the "mark historical, carry start/end" contract.
func TestBuildHistoricalMonitorEnvelopeMarksWindowRange(t *testing.T) {
	env := BuildHistoricalMonitorEnvelope([]entity.InstanceSnapshot{{UHostId: "uhost-a"}}, []Metric{MetricCPU}, monitorAPIResult(), 1777442400, 1777444200)
	require.NotEmpty(t, env.Facts, "historical monitor facts are present")
	for _, f := range env.Facts {
		assert.Equal(t, "range", f.Period, "historical facts are a range, not latest")
		assert.Equal(t, int64(1777442400), f.WindowStart)
		assert.Equal(t, int64(1777444200), f.WindowEnd)
	}
}
