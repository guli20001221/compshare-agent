package intent

import (
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

func assertEnvelopeFact(t *testing.T, env envelope.Envelope, subjectID, key string, value any) {
	t.Helper()
	for _, fact := range env.Facts {
		if fact.SubjectID == subjectID && fact.Key == key {
			assert.Equal(t, value, fact.Value)
			assert.Equal(t, envelope.FactSourceAPI, fact.Source)
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
