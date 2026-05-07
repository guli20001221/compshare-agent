package entity

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveByID_Statuses(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	reg := NewRegistry(WithClock(func() time.Time { return now }))

	require.NoError(t, reg.SyncFromDescribe(describeResult(
		host("uhost-live", "train-a", "Running", "4090", 1),
		host("uhost-old", "old-gpu", "Stopped", "A100", 1),
	), "init"))

	got, res := reg.ResolveByID("uhost-live")
	require.NotNil(t, got)
	assert.Equal(t, ResolveHit, res.Status)
	assert.Equal(t, "train-a", got.Name)

	now = now.Add(2 * time.Minute)
	require.NoError(t, reg.SyncFromDescribe(describeResult(
		host("uhost-live", "train-a", "Running", "4090", 1),
	), "describe_success"))

	missing, res := reg.ResolveByID("uhost-missing")
	assert.Nil(t, missing)
	assert.Equal(t, ResolveNotFoundInAccount, res.Status)

	released, res := reg.ResolveByID("uhost-old")
	assert.Nil(t, released)
	assert.Equal(t, ResolveRecentlyReleasedGuess, res.Status)
}

func TestResolveByName_UniqueAmbiguousAndFuzzyStableOrder(t *testing.T) {
	reg := NewRegistry()
	require.NoError(t, reg.SyncFromDescribe(describeResult(
		host("uhost-a", "train-a", "Running", "4090", 1),
		host("uhost-b", "train-b", "Running", "A100", 1),
		host("uhost-c", "train-b", "Stopped", "A100", 1),
		host("uhost-d", "qa-shadow-20260417-4090", "Running", "4090", 1),
		host("uhost-e", "shadow-test-4090", "Running", "4090", 1),
	), "init"))

	matches, res := reg.ResolveByName("train-a")
	require.Equal(t, ResolveHit, res.Status)
	require.Len(t, matches, 1)
	assert.Equal(t, "uhost-a", matches[0].UHostId)

	matches, res = reg.ResolveByName("train-b")
	assert.Equal(t, ResolveAmbiguous, res.Status)
	assert.Equal(t, []string{"uhost-b", "uhost-c"}, idsOf(matches))

	matches, res = reg.ResolveByName("shadow 4090")
	assert.Equal(t, ResolveAmbiguous, res.Status)
	assert.Equal(t, []string{"uhost-d", "uhost-e"}, idsOf(matches), "fuzzy order must be stable")
}

func TestResolveByName_NormalizesChinesePunctuation(t *testing.T) {
	reg := NewRegistry()
	require.NoError(t, reg.SyncFromDescribe(describeResult(
		host("uhost-cn", "训练、4090", "Running", "4090", 1),
	), "init"))

	matches, res := reg.ResolveByName("训练4090")
	require.Equal(t, ResolveHit, res.Status)
	require.Len(t, matches, 1)
	assert.Equal(t, "uhost-cn", matches[0].UHostId)
}

func TestFilter_ByStateAndGPUType(t *testing.T) {
	reg := NewRegistry()
	require.NoError(t, reg.SyncFromDescribe(describeResult(
		host("uhost-a", "train-a", "Running", "4090", 1),
		host("uhost-b", "train-b", "Stopped", "4090", 1),
		host("uhost-c", "train-c", "Running", "A100", 1),
		host("uhost-d", "no-card", "Running", "4090", 0),
	), "init"))

	running := reg.Filter(FilterSpec{State: "Running"})
	assert.Equal(t, []string{"uhost-a", "uhost-c", "uhost-d"}, idsOf(running))

	gpu4090 := reg.Filter(FilterSpec{GPUType: "4090"})
	assert.Equal(t, []string{"uhost-a", "uhost-b", "uhost-d"}, idsOf(gpu4090))

	running4090 := reg.Filter(FilterSpec{State: "Running", GPUType: "4090"})
	assert.Equal(t, []string{"uhost-a", "uhost-d"}, idsOf(running4090))
}

func TestSyncMetadataAndAge(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	reg := NewRegistry(WithClock(func() time.Time { return now }))

	require.NoError(t, reg.SyncFromDescribe(describeResult(
		host("uhost-a", "train-a", "Running", "4090", 1),
	), "init"))

	assert.Equal(t, "init", reg.LastSyncEvent)
	assert.Equal(t, now, reg.LastFullSync)
	now = now.Add(90 * time.Second)
	assert.Equal(t, 90*time.Second, reg.Age())
}

func TestSyncFromDescribeParsesJSONNumberFields(t *testing.T) {
	reg := NewRegistry()
	require.NoError(t, reg.SyncFromDescribe(map[string]any{
		"TotalCount": json.Number("1"),
		"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-json",
				"Name":       "json-number",
				"State":      "Running",
				"GPU":        json.Number("2"),
				"CPU":        json.Number("32"),
				"Memory":     json.Number("131072"),
				"ExpireTime": json.Number("1778148600"),
			},
		},
	}, "init"))

	got, res := reg.ResolveByID("uhost-json")
	require.Equal(t, ResolveHit, res.Status)
	require.NotNil(t, got)
	assert.Equal(t, 2, got.GPU)
	assert.Equal(t, 32, got.CPU)
	assert.Equal(t, 131072, got.Memory)
	assert.Equal(t, int64(1778148600), got.ExpireTime)
	assert.Equal(t, 1, reg.TotalCount)
}

func describeResult(hosts ...map[string]any) map[string]any {
	set := make([]any, 0, len(hosts))
	for _, h := range hosts {
		set = append(set, h)
	}
	return map[string]any{
		"RetCode":    float64(0),
		"TotalCount": float64(len(hosts)),
		"UHostSet":   set,
	}
}

func host(id, name, state, gpuType string, gpu int) map[string]any {
	return map[string]any{
		"UHostId":    id,
		"Name":       name,
		"State":      state,
		"GpuType":    gpuType,
		"GPU":        float64(gpu),
		"CPU":        float64(16),
		"Memory":     float64(65536),
		"OsType":     "Linux",
		"Zone":       "cn-wlcb-01",
		"Region":     "cn-wlcb",
		"ChargeType": "Dynamic",
		"ExpireTime": float64(1778148600),
		"AutoRenew":  "No",
	}
}

func idsOf(items []*InstanceSnapshot) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.UHostId)
	}
	return ids
}
