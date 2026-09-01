package readprojection

import (
	"strings"
	"testing"

	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/envelope"
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
		UHostId: "uhost-a",
		Name:    "train-a",
		State:   "Running",
		GpuType: "4090",
		GPU:     1,
		CPU:     8,
		// MB, as upstream reports it. The fixture used to say 64 and still expect
		// "64 GB", which only passed because the converter treated small values as
		// already-GB — a fixture that encoded the wrong unit and a converter that
		// agreed with it.
		Memory:     65536,
		ImageType:  "Ubuntu",
		StartTime:  1000,
		ExpireTime: 2000,
	}}

	got := RenderResourceSummary(instances, ResourceEnvelopeMeta{})

	want := "- train-a（uhost-a）：运行中；4090 × 1；8 vCPU / 64 GB；镜像 Ubuntu；启动于 1970-01-01 08:16；到期于 1970-01-01 08:33"
	require.Equal(t, want, got)
}

func TestRenderResourceSummaryIncludesCurrentImageShapeAndLifecycleTimes(t *testing.T) {
	got := RenderResourceSummary([]entity.InstanceSnapshot{{
		UHostId: "cpod-a", State: "Stopped", CPU: 2, Memory: 4096,
		ImageName: "PyTorch 2.9", ImageType: "App", InstanceType: "Container",
		SchedulerStopTime: 1000, StopTime: 2000, ReleaseTime: 3000,
	}}, ResourceEnvelopeMeta{})

	assert.Contains(t, got, "镜像 PyTorch 2.9")
	assert.Contains(t, got, "镜像类型 App")
	assert.Contains(t, got, "实例类型 Container")
	assert.Contains(t, got, "计划关机于 1970-01-01 08:16")
	assert.Contains(t, got, "关机于 1970-01-01 08:33")
	assert.Contains(t, got, "预计回收于 1970-01-01 08:50")
}

func TestRenderResourceSummaryIncludesCFSAndMigrationProgressWhenReturned(t *testing.T) {
	got := RenderResourceSummary([]entity.InstanceSnapshot{{
		UHostId: "cpod-a", State: "Migrating", CPU: 2, Memory: 4096, CfsID: "cfs-1",
		MigrationProgress: entity.InstanceMigrationProgress{
			Present: true, MigrationID: "migration-1", State: "Running", Current: "88.8G",
			Total: "100.0G", Speed: "1.2G/s", ETASeconds: 10, Percent: 88,
		},
	}}, ResourceEnvelopeMeta{})

	assert.Contains(t, got, "挂载 CFS cfs-1")
	assert.Contains(t, got, "系统盘迁移 Running（88%），88.8G/100.0G，速度 1.2G/s，预计剩余 10 秒")
}

func TestRenderResourceSummaryOmitsAbsentMigrationProgress(t *testing.T) {
	got := RenderResourceSummary([]entity.InstanceSnapshot{{
		UHostId: "uhost-a", State: "Running", CPU: 2, Memory: 4096,
	}}, ResourceEnvelopeMeta{})

	assert.NotContains(t, got, "系统盘迁移")
}

func TestRenderResourceSummary_NoGPUDoesNotAdvertiseTheStoredGPUModel(t *testing.T) {
	got := RenderResourceSummary([]entity.InstanceSnapshot{{
		UHostId: "uhost-a", State: "Running", GpuType: "4090", GPU: 0, CPU: 2, Memory: 4096,
	}}, ResourceEnvelopeMeta{})

	assert.Contains(t, got, "无 GPU")
	assert.NotContains(t, got, "4090")
}

func TestRenderResourceSummaryUsesOnlyTheLiveZoneCatalogLabel(t *testing.T) {
	instance := []entity.InstanceSnapshot{{
		UHostId: "uhost-1", State: "Running", Zone: "cn-wlcb-01", CPU: 2, Memory: 4096,
	}}
	catalog := deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{{
		Placement: deployment.ZonePlacement{Zone: "cn-wlcb-01"}, DisplayName: "华北二A",
	}})

	withCatalog := RenderResourceSummaryWithZoneCatalog(instance, ResourceEnvelopeMeta{}, catalog)
	assert.Contains(t, withCatalog, "可用区 华北二A（cn-wlcb-01）")
	assert.NotContains(t, withCatalog, "华北一C")

	withoutCatalog := RenderResourceSummaryWithZoneCatalog(instance, ResourceEnvelopeMeta{}, nil)
	assert.Contains(t, withoutCatalog, "可用区 cn-wlcb-01")
	assert.NotContains(t, withoutCatalog, "华北")
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

	assert.Equal(t, "- CPU 使用率：12.5%\n- GPU 使用率：87%", got)
}

// TestMonitorMetricsAreOnePerLine is why the join changed. A live instance
// returns eight scalar metrics (CPU, memory, GPU, VRAM, system disk, one row per
// data disk); as a single "; "-joined run they were a 90-character line the
// reader had to parse by eye, while this block is force-inserted verbatim into
// the answer (presentation=required) — what is built here is exactly what ships.
func TestMonitorMetricsAreOnePerLine(t *testing.T) {
	got := RenderMonitorSummary([]Metric{MetricCPU, MetricGPU}, monitorAPIResult())

	lines := strings.Split(got, "\n")
	assert.Len(t, lines, 2, "one metric per line")
	for _, line := range lines {
		assert.True(t, strings.HasPrefix(line, "- "), "every row is a Markdown list item: %q", line)
	}
	assert.NotContains(t, got, "; ", "the flat one-line join must not come back")
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

	assert.Contains(t, got, "- CPU 使用率：8%")
	assert.Contains(t, got, "显存使用率未返回数据")
}

func TestRenderMonitorSummaryEmptyIsNoValues(t *testing.T) {
	assert.Equal(t, "未返回监控数据。", RenderMonitorSummary(nil, map[string]any{}))
}

// TestExtractMonitorScalarsSharesRendererVocabulary pins the scalar keys used
// by monitor rendering and read-side instance reference tracking.
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
	assert.Contains(t, got, "北京时间 2026-04-29 14:00 ~ 2026-04-29 14:30（历史时间窗）", "the reply states the exact queried window")
	assert.Contains(t, got, "未返回监控数据", "empty payload is window-scoped no-data, never a fabricated 0%/healthy value")
}

func TestHistoricalBatchKeepsSeriesOwnershipAndNamesEmptySubject(t *testing.T) {
	payload := map[string]any{"Data": map[string]any{"List": []any{
		map[string]any{
			"UHostId": "uhost-a",
			"Metrics": []any{monitorMetric("uhost_cpu_used", nil, 12)},
		},
		map[string]any{"UHostId": "uhost-b", "Metrics": []any{}},
	}}}

	got := RenderHistoricalMonitorSummary([]Metric{MetricCPU}, payload, 1777442400, 1777444200)
	assert.Contains(t, got, "uhost-a · CPU 使用率")
	assert.Contains(t, got, "uhost-b · CPU 使用率未返回数据")

	env := BuildHistoricalMonitorEnvelope([]entity.InstanceSnapshot{{UHostId: "uhost-a"}, {UHostId: "uhost-b"}}, []Metric{MetricCPU}, payload, 1777442400, 1777444200)
	foundAbsence := false
	for _, fact := range env.Facts {
		if fact.SubjectID == "uhost-b" && fact.Key == "missing_cpu_usage" && fact.Value == "未返回数据" {
			foundAbsence = true
		}
	}
	assert.True(t, foundAbsence, "the structured observation must preserve the empty subject")
}

func TestHistoricalMonitorNamesAMissingRequestedMetricBesidePresentData(t *testing.T) {
	payload := map[string]any{"Data": map[string]any{"List": []any{
		map[string]any{
			"UHostId": "uhost-a",
			"Metrics": []any{monitorMetric("uhost_cpu_used", nil, 8)},
		},
	}}}

	got := RenderHistoricalMonitorSummary([]Metric{MetricCPU, MetricGPU}, payload, 1777442400, 1777444200)
	assert.Contains(t, got, "- CPU 使用率：最新 8%")
	assert.Contains(t, got, "- GPU 使用率未返回数据")

	env := BuildHistoricalMonitorEnvelope([]entity.InstanceSnapshot{{UHostId: "uhost-a"}}, []Metric{MetricCPU, MetricGPU}, payload, 1777442400, 1777444200)
	assertEnvelopeFact(t, env, "uhost-a", "cpu_usage", "8")
	assertEnvelopeFactWithSource(t, env, "uhost-a", "missing_gpu_usage", "未返回数据", envelope.FactSourceComputed)
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
