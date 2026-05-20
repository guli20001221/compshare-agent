package intent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/envelope"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildResourceEnvelopeIsStableAndCustomerSafe(t *testing.T) {
	instances := []entity.InstanceSnapshot{
		{
			UHostId:    "uhost-b",
			Name:       "Authorization: Bearer " + strings.Repeat("b", 25),
			State:      "Stopped",
			OsType:     "Linux",
			GPU:        1,
			GpuType:    "4090",
			ImageType:  "Ubuntu",
			StartTime:  2000,
			CPU:        8,
			Memory:     64,
			Zone:       "cn-wlcb-01",
			Region:     "cn-wlcb",
			ChargeType: "Dynamic",
			ExpireTime: 3000,
			AutoRenew:  "No",
		},
		{
			UHostId:    "uhost-a",
			Name:       "train-a",
			State:      "Running",
			OsType:     "Linux",
			GPU:        2,
			GpuType:    "A100",
			ImageType:  "CentOS",
			StartTime:  1000,
			CPU:        16,
			Memory:     128,
			Zone:       "cn-wlcb-01",
			Region:     "cn-wlcb",
			ChargeType: "Postpay",
			AutoRenew:  "Yes",
		},
	}

	env := BuildResourceEnvelope(instances)
	hash, err := envelope.Hash(env)
	require.NoError(t, err)

	assert.Equal(t, envelope.KindResourceInfo, env.Kind)
	assert.Equal(t, []string{"DescribeCompShareInstance"}, env.SourceActions)
	require.Len(t, env.Subjects, 2)
	assert.Equal(t, "uhost-a", env.Subjects[0].ID)
	assert.Equal(t, "uhost-b", env.Subjects[1].ID)
	assert.True(t, env.Constraints.DoNotInventInstances)
	assert.True(t, env.Constraints.DoNotAnswerAccountBill)
	assert.False(t, env.Constraints.DoNotInventMetrics)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, hash)

	assertEnvelopeFact(t, env, "uhost-a", "name", "train-a")
	assertEnvelopeFact(t, env, "uhost-a", "state", "Running")
	assertEnvelopeFact(t, env, "uhost-a", "charge_type", "Postpay")
	assertEnvelopeFact(t, env, "uhost-b", "auto_renew", "No")
	assertEnvelopeFact(t, env, "uhost-b", "name", "Authorization: Bearer [REDACTED]")
	assertNoEnvelopeFact(t, env, "uhost-a", "expire_time")
	assert.NotContains(t, hash, strings.Repeat("b", 25))
}

func TestBuildResourceEnvelopeWithFilterMeta(t *testing.T) {
	instances := []entity.InstanceSnapshot{{
		UHostId: "uhost-a",
		Name:    "run-a",
		State:   "Running",
	}}

	env := BuildResourceEnvelopeWithMeta(instances, ResourceEnvelopeMeta{
		FilterApplied: "state=running",
		MatchedCount:  1,
		TotalCount:    3,
	})

	require.Len(t, env.Subjects, 1)
	assert.Equal(t, "uhost-a", env.Subjects[0].ID)
	assertEnvelopeComputedFact(t, env, "filter_applied", "state=running")
	assertEnvelopeComputedFact(t, env, "matched_count", "1")
	assertEnvelopeComputedFact(t, env, "total_count", "3")
}

func TestBuildResourceEnvelopeWithTotalCountMeta(t *testing.T) {
	instances := []entity.InstanceSnapshot{{
		UHostId: "uhost-a",
		Name:    "run-a",
		State:   "Running",
	}}

	env := BuildResourceEnvelopeWithMeta(instances, ResourceEnvelopeMeta{TotalCount: 16})

	assertEnvelopeComputedFact(t, env, "total_count", "16")
	assertNoEnvelopeComputedFact(t, env, "matched_count")
	assertNoEnvelopeComputedFact(t, env, "filter_applied")
}

func TestBuildMonitorEnvelopeUsesResolvedSubjectsAndFiltersMetrics(t *testing.T) {
	subjects := []entity.InstanceSnapshot{{
		UHostId: "uhost-a",
		Name:    "train-a",
		State:   "Running",
	}}
	payload := map[string]any{
		"CPU": float64(12.5),
		"GPU": float64(87),
		"Nested": map[string]any{
			"Authorization": "Bearer " + strings.Repeat("c", 25),
		},
	}

	env := BuildMonitorEnvelope(subjects, []Metric{MetricGPU}, payload)

	assert.Equal(t, envelope.KindMonitorQuery, env.Kind)
	assert.Equal(t, []string{"GetCompShareInstanceMonitor"}, env.SourceActions)
	require.Len(t, env.Subjects, 1)
	assert.Equal(t, "uhost-a", env.Subjects[0].ID)
	assert.Equal(t, "train-a", env.Subjects[0].Name)
	assert.Contains(t, envelope.AllowedIDs(env), "uhost-a")
	assert.True(t, env.Constraints.DoNotInventInstances)
	assert.True(t, env.Constraints.DoNotInventMetrics)
	assert.True(t, env.Constraints.DoNotAnswerAccountBill)

	require.Len(t, env.Facts, 1)
	assert.Equal(t, "GPU", env.Facts[0].Key)
	assert.Equal(t, "uhost-a", env.Facts[0].SubjectID)
	assert.Equal(t, "latest", env.Facts[0].Period)
	assert.Equal(t, "latest", env.Facts[0].Aggregation)
	assert.NotContains(t, env.Facts[0].Value, strings.Repeat("c", 25))
}

func TestBuildMonitorEnvelopeExtractsSemanticMonitorFacts(t *testing.T) {
	subjects := []entity.InstanceSnapshot{{
		UHostId: "uhost-a",
		Name:    "train-a",
		State:   "Running",
	}}

	env := BuildMonitorEnvelope(subjects, []Metric{MetricCPU, MetricGPU}, monitorAPIResult())

	require.Len(t, env.Facts, 2)
	assertEnvelopeFact(t, env, "uhost-a", "cpu_usage", "12.5")
	assertEnvelopeFact(t, env, "uhost-a", "gpu_usage", "87")
	assertNoEnvelopeFact(t, env, "uhost-a", "system_disk_usage")
	assertNoEnvelopeFact(t, env, "uhost-a", "data_disk_usage")
	assertNoEnvelopeFact(t, env, "uhost-a", "memory_usage")
	assertNoEnvelopeFact(t, env, "uhost-a", "vram_usage")
	for _, fact := range env.Facts {
		assert.Equal(t, "%", fact.Unit)
		assert.Equal(t, "latest", fact.Period)
		assert.Equal(t, "latest", fact.Aggregation)
		assert.NotContains(t, fact.Key, "TagMap")
		assert.NotContains(t, fact.Key, "00:03.0")
	}
}

func TestBuildMonitorEnvelopeIncludesMissingRequestedMetricFacts(t *testing.T) {
	subjects := []entity.InstanceSnapshot{{
		UHostId: "uhost-a",
		Name:    "train-a",
		State:   "Running",
	}}
	payload := map[string]any{
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
	}

	env := BuildMonitorEnvelope(subjects, []Metric{MetricCPU, MetricVRAM}, payload)

	assertEnvelopeFact(t, env, "uhost-a", "cpu_usage", "8")
	assertEnvelopeFactWithSource(t, env, "uhost-a", "missing_vram_usage", "未返回数据", envelope.FactSourceComputed)
}

func TestBuildMonitorEnvelopeUsesOrdinalLabelsForMultiGPUFacts(t *testing.T) {
	subjects := []entity.InstanceSnapshot{{UHostId: "uhost-a", Name: "train-a"}}
	payload := map[string]any{
		"Data": map[string]any{
			"List": []any{
				map[string]any{
					"UHostId": "uhost-a",
					"Metrics": []any{
						map[string]any{
							"MetricKey": "cloudwatch_gpu_util",
							"Results": []any{
								map[string]any{"TagMap": map[string]any{"gpu_bus_id": "00:03.0"}, "Values": []any{map[string]any{"Value": float64(11)}}},
								map[string]any{"TagMap": map[string]any{"gpu_bus_id": "00:04.0"}, "Values": []any{map[string]any{"Value": float64(22)}}},
							},
						},
					},
				},
			},
		},
	}

	env := BuildMonitorEnvelope(subjects, []Metric{MetricGPU}, payload)

	assertEnvelopeFact(t, env, "uhost-a", "gpu_usage.GPU 1", "11")
	assertEnvelopeFact(t, env, "uhost-a", "gpu_usage.GPU 2", "22")
	for _, fact := range env.Facts {
		assert.NotContains(t, fact.Label, "00:")
		assert.NotContains(t, fact.Key, "00:")
	}
}

func TestBuildMonitorEnvelopeRecognizedAPIShapeDoesNotFallbackToRawMetadata(t *testing.T) {
	subjects := []entity.InstanceSnapshot{{UHostId: "uhost-a", Name: "train-a"}}
	payload := map[string]any{
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
	}

	env := BuildMonitorEnvelope(subjects, []Metric{MetricGPU}, payload)

	assertEnvelopeFactWithSource(t, env, "uhost-a", "missing_gpu_usage", "未返回数据", envelope.FactSourceComputed)
	raw, err := json.Marshal(env)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "gpu_bus_id")
	assert.NotContains(t, string(raw), "00:03.0")
}

func TestBuildMonitorEnvelopeHashIsStable(t *testing.T) {
	subjects := []entity.InstanceSnapshot{{UHostId: "uhost-a", Name: "train-a"}}
	left := BuildMonitorEnvelope(subjects, nil, map[string]any{"GPU": 87, "CPU": 1})
	right := BuildMonitorEnvelope(subjects, nil, map[string]any{"CPU": 1, "GPU": 87})

	leftHash, err := envelope.Hash(left)
	require.NoError(t, err)
	rightHash, err := envelope.Hash(right)
	require.NoError(t, err)

	assert.Equal(t, leftHash, rightHash)
}

func monitorAPIResult() map[string]any {
	return map[string]any{
		"Data": map[string]any{
			"List": []any{
				map[string]any{
					"UHostId": "uhost-a",
					"Metrics": []any{
						monitorMetric("uhost_cpu_used", nil, 7, 12.5),
						monitorMetric("cloudwatch_memory_usage", nil, 4),
						monitorMetric("cloudwatch_sys_disk_used_per", map[string]any{"mount": "/dev/vda1"}, 45),
						monitorMetric("cloudwatch_data_disk_used_per", map[string]any{"mount": "/data"}, 55),
						monitorMetric("cloudwatch_gpu_util", map[string]any{"gpu_bus_id": "00:03.0"}, 0, 87),
						monitorMetric("cloudwatch_gpu_memory_usage", map[string]any{"gpu_bus_id": "00:03.0"}, 72),
					},
				},
			},
		},
	}
}

func monitorMetric(key string, tags map[string]any, values ...float64) map[string]any {
	points := make([]any, 0, len(values))
	for i, value := range values {
		points = append(points, map[string]any{
			"Timestamp": float64(1778420000 + i),
			"Value":     value,
		})
	}
	result := map[string]any{"Values": points}
	if tags != nil {
		result["TagMap"] = tags
	}
	return map[string]any{
		"MetricKey": key,
		"Results":   []any{result},
	}
}

func assertEnvelopeFact(t *testing.T, env envelope.Envelope, subjectID, key string, value any) {
	t.Helper()
	assertEnvelopeFactWithSource(t, env, subjectID, key, value, envelope.FactSourceAPI)
}

func assertEnvelopeFactWithSource(t *testing.T, env envelope.Envelope, subjectID, key string, value any, source envelope.FactSource) {
	t.Helper()
	for _, fact := range env.Facts {
		if fact.SubjectID == subjectID && fact.Key == key {
			assert.Equal(t, value, fact.Value)
			assert.Equal(t, source, fact.Source)
			return
		}
	}
	t.Fatalf("missing fact subject=%s key=%s in %#v", subjectID, key, env.Facts)
}

func assertNoEnvelopeFact(t *testing.T, env envelope.Envelope, subjectID, key string) {
	t.Helper()
	for _, fact := range env.Facts {
		if fact.SubjectID == subjectID && fact.Key == key {
			t.Fatalf("unexpected fact subject=%s key=%s in %#v", subjectID, key, env.Facts)
		}
	}
}

func assertEnvelopeComputedFact(t *testing.T, env envelope.Envelope, key string, value any) {
	t.Helper()
	for _, fact := range env.Computed {
		if fact.Key == key {
			assert.Equal(t, value, fact.Value)
			assert.Equal(t, envelope.FactSourceComputed, fact.Source)
			return
		}
	}
	t.Fatalf("missing computed fact key=%s in %#v", key, env.Computed)
}

func assertNoEnvelopeComputedFact(t *testing.T, env envelope.Envelope, key string) {
	t.Helper()
	for _, fact := range env.Computed {
		if fact.Key == key {
			t.Fatalf("unexpected computed fact key=%s in %#v", key, env.Computed)
		}
	}
}
