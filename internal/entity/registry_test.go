package entity

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
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

	snap := reg.Snapshot()
	assert.Equal(t, "init", snap.SyncEvent)
	assert.Equal(t, now, snap.LastFullSync)
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
	assert.Equal(t, 1, reg.Snapshot().TotalCount)
}

func TestSyncFromDescribeParsesValidatorSnapshotFields(t *testing.T) {
	reg := NewRegistry()
	require.NoError(t, reg.SyncFromDescribe(describeResult(map[string]any{
		"UHostId":   "uhost-validator",
		"Name":      "validator-host",
		"State":     "Running",
		"ImageType": "Custom",
		"StartTime": float64(1778145000),
	}), "init"))

	got, res := reg.ResolveByID("uhost-validator")
	require.Equal(t, ResolveHit, res.Status)
	require.NotNil(t, got)
	assert.Equal(t, "Custom", got.ImageType)
	assert.Equal(t, int64(1778145000), got.StartTime)
}

func TestSnapshotReturnsDeepCopies(t *testing.T) {
	reg := NewRegistry()
	require.NoError(t, reg.SyncFromDescribe(describeResult(
		host("uhost-a", "train-a", "Running", "4090", 1),
		host("uhost-b", "train-b", "Stopped", "A100", 1),
	), "init"))

	snap := reg.Snapshot()
	require.NotEmpty(t, snap.SnapshotID)
	snap.Instances["uhost-a"] = InstanceSnapshot{UHostId: "uhost-a", Name: "mutated"}
	snap.NameIndex[normalizeName("train-a")][0] = "uhost-mutated"
	delete(snap.Instances, "uhost-b")

	got, res := reg.ResolveByID("uhost-a")
	require.Equal(t, ResolveHit, res.Status)
	require.NotNil(t, got)
	assert.Equal(t, "train-a", got.Name)

	matches, res := reg.ResolveByName("train-a")
	require.Equal(t, ResolveHit, res.Status)
	require.Len(t, matches, 1)
	assert.Equal(t, "uhost-a", matches[0].UHostId)

	got, res = reg.ResolveByID("uhost-b")
	require.Equal(t, ResolveHit, res.Status)
	require.NotNil(t, got)
}

func TestSnapshotIDStableAcrossInputOrder(t *testing.T) {
	regA := NewRegistry()
	require.NoError(t, regA.SyncFromDescribe(describeResult(
		host("uhost-a", "train-a", "Running", "4090", 1),
		host("uhost-b", "train-b", "Stopped", "A100", 1),
	), "init"))

	regB := NewRegistry()
	require.NoError(t, regB.SyncFromDescribe(describeResult(
		host("uhost-b", "train-b", "Stopped", "A100", 1),
		host("uhost-a", "train-a", "Running", "4090", 1),
	), "init"))

	assert.Equal(t, regA.Snapshot().SnapshotID, regB.Snapshot().SnapshotID)
}

func TestConcurrentResolveAndSyncFromDescribeNoRace(t *testing.T) {
	reg := NewRegistry()
	require.NoError(t, reg.SyncFromDescribe(describeResult(
		host("uhost-a", "train-a", "Running", "4090", 1),
		host("uhost-b", "train-b", "Stopped", "A100", 1),
	), "init"))

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 200; j++ {
				_, _ = reg.ResolveByID("uhost-a")
				_, _ = reg.ResolveByName("train")
				_ = reg.Filter(FilterSpec{GPUType: "4090"})
				_ = reg.Snapshot()
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for j := 0; j < 80; j++ {
			hosts := []map[string]any{
				host("uhost-a", "train-a", "Running", "4090", 1),
				host("uhost-b", "train-b", "Stopped", "A100", 1),
			}
			if j%2 == 0 {
				hosts = append(hosts, host("uhost-c", "train-c", "Running", "H20", 1))
			}
			require.NoError(t, reg.SyncFromDescribe(describeResult(hosts...), "sync_refresh"))
		}
	}()

	close(start)
	wg.Wait()
}

func TestTraceStateInitialUnavailable(t *testing.T) {
	reg := NewRegistry()

	state := reg.TraceState(time.Date(2026, 5, 9, 10, 0, 0, 0, time.UTC))

	assert.Equal(t, "", state.SnapshotID)
	assert.Equal(t, int64(0), state.AgeSeconds)
	assert.Equal(t, string(SyncEventUnavailable), state.SyncEvent)
	assert.True(t, reg.NeedsRefresh(time.Now()))
}

func TestRefreshRecordsSyncEvents(t *testing.T) {
	ctx := context.Background()
	exec := &registryRefreshExecutor{result: describeResult(
		host("uhost-a", "train-a", "Running", "4090", 1),
	)}

	reg := NewRegistry()
	require.NoError(t, reg.Refresh(ctx, exec, RefreshReasonInit))
	assert.Equal(t, string(SyncEventInit), reg.Snapshot().SyncEvent)
	assert.NotEmpty(t, reg.Snapshot().SnapshotID)

	require.NoError(t, reg.Refresh(ctx, exec, RefreshReasonManual))
	assert.Equal(t, string(SyncEventSyncRefresh), reg.Snapshot().SyncEvent)

	require.NoError(t, reg.Refresh(ctx, exec, RefreshReasonTTL))
	assert.Equal(t, string(SyncEventSyncRefresh), reg.Snapshot().SyncEvent)
}

func TestWarmRefreshRecordsWarmCache(t *testing.T) {
	reg := NewRegistry()
	errCh := reg.WarmRefresh(context.Background(), &registryRefreshExecutor{result: describeResult(
		host("uhost-warm", "warm-cache", "Running", "H20", 1),
	)})

	require.NoError(t, <-errCh)
	assert.Equal(t, string(SyncEventWarmCache), reg.Snapshot().SyncEvent)
}

func TestRefreshFailurePreservesPreviousSnapshot(t *testing.T) {
	ctx := context.Background()
	reg := NewRegistry()
	require.NoError(t, reg.Refresh(ctx, &registryRefreshExecutor{result: describeResult(
		host("uhost-a", "train-a", "Running", "4090", 1),
	)}, RefreshReasonInit))
	before := reg.Snapshot()

	err := reg.Refresh(ctx, &registryRefreshExecutor{err: errors.New("platform timeout")}, RefreshReasonManual)

	require.Error(t, err)
	after := reg.Snapshot()
	assert.Equal(t, before.SnapshotID, after.SnapshotID)
	assert.Equal(t, string(SyncEventFailed), after.SyncEvent)
	assert.Equal(t, "timeout", after.LastSyncError)
	assert.True(t, reg.NeedsRefresh(time.Now()), "failed refresh must not be suppressed by a still-fresh previous snapshot")
	got, res := reg.ResolveByID("uhost-a")
	require.Equal(t, ResolveHit, res.Status)
	require.NotNil(t, got)
}

func TestRefreshFailureWithoutPreviousSnapshot(t *testing.T) {
	reg := NewRegistry()

	err := reg.Refresh(context.Background(), &registryRefreshExecutor{err: errors.New("network down")}, RefreshReasonInit)

	require.Error(t, err)
	snap := reg.Snapshot()
	assert.Equal(t, "", snap.SnapshotID)
	assert.Equal(t, string(SyncEventFailed), snap.SyncEvent)
	assert.Equal(t, "network", snap.LastSyncError)
}

func TestRefreshParseFailureRecordsFailed(t *testing.T) {
	reg := NewRegistry()

	err := reg.Refresh(context.Background(), &registryRefreshExecutor{result: map[string]any{
		"TotalCount": 0,
	}}, RefreshReasonInit)

	require.Error(t, err)
	snap := reg.Snapshot()
	assert.Equal(t, "", snap.SnapshotID)
	assert.Equal(t, string(SyncEventFailed), snap.SyncEvent)
	assert.Equal(t, "parse_error", snap.LastSyncError)
	assert.True(t, reg.NeedsRefresh(time.Now()))
}

func TestNeedsRefreshAndInvalidationWhitelist(t *testing.T) {
	now := time.Date(2026, 5, 9, 10, 0, 0, 0, time.UTC)
	reg := NewRegistry(WithClock(func() time.Time { return now }))
	require.NoError(t, reg.Refresh(context.Background(), &registryRefreshExecutor{result: describeResult(
		host("uhost-a", "train-a", "Running", "4090", 1),
	)}, RefreshReasonInit))

	assert.False(t, reg.NeedsRefresh(now.Add(29*time.Second)))
	assert.True(t, reg.NeedsRefresh(now.Add(31*time.Second)))

	invalidateActions := []string{
		"CreateCompShareInstance",
		"CreateInstanceWorkflow",
		"StartCompShareInstance",
		"StopCompShareInstance",
		"RebootCompShareInstance",
		"StartInstanceWorkflow",
		"StopInstanceWorkflow",
		"RebootInstanceWorkflow",
		"ModifyCompShareInstanceName",
		"RenameInstanceWorkflow",
		"UpdateCompShareStopScheduler",
		"DeleteCompShareStopScheduler",
		"SetStopSchedulerWorkflow",
		"CancelStopSchedulerWorkflow",
	}
	for _, action := range invalidateActions {
		t.Run(action, func(t *testing.T) {
			reg := NewRegistry(WithClock(func() time.Time { return now }))
			require.NoError(t, reg.Refresh(context.Background(), &registryRefreshExecutor{result: describeResult(
				host("uhost-a", "train-a", "Running", "4090", 1),
			)}, RefreshReasonInit))
			assert.True(t, reg.MarkInvalidated(action))
			assert.True(t, reg.NeedsRefresh(now.Add(time.Second)))
		})
	}

	nonInvalidatingActions := []string{
		"TerminateCompShareInstance",
		"ResetCompShareInstancePassword",
		"ResetPasswordWorkflow",
		"CreateCompShareCustomImage",
		"UpdateCompShareTeam",
	}
	for _, action := range nonInvalidatingActions {
		t.Run(action, func(t *testing.T) {
			reg := NewRegistry(WithClock(func() time.Time { return now }))
			require.NoError(t, reg.Refresh(context.Background(), &registryRefreshExecutor{result: describeResult(
				host("uhost-a", "train-a", "Running", "4090", 1),
			)}, RefreshReasonInit))
			assert.False(t, reg.MarkInvalidated(action))
			assert.False(t, reg.NeedsRefresh(now.Add(time.Second)))
		})
	}
}

type registryRefreshExecutor struct {
	result map[string]any
	err    error
}

func (e *registryRefreshExecutor) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	if e.err != nil {
		return nil, e.err
	}
	if action != "DescribeCompShareInstance" {
		return nil, errors.New("unexpected action")
	}
	if args["Limit"] != 100 {
		return nil, errors.New("unexpected limit")
	}
	return e.result, nil
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
