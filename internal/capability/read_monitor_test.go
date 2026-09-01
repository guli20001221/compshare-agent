package capability

import (
	"context"
	"errors"
	"testing"

	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/platform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// monitorFixture is a recognized GetCompShareInstanceMonitor payload carrying a
// single cpu_usage sample for the given host.
func monitorFixture(uhostID string) map[string]any {
	return map[string]any{
		"Data": map[string]any{
			"List": []any{
				map[string]any{
					"UHostId": uhostID,
					"Metrics": []any{
						map[string]any{
							"MetricKey": "uhost_cpu_used",
							"Results": []any{map[string]any{
								"Values": []any{map[string]any{"Value": float64(12.5), "Timestamp": float64(1778420000)}},
							}},
						},
					},
				},
			},
		},
	}
}

func runMonitorCurrent(t *testing.T, exec ReadExecutor, resolver EntityResolver, fallbackID string, req MonitorCurrentRequest) ReadResult {
	t.Helper()
	reg := NewReadCapability(monitorCurrentReadSpec())
	return reg.Run(context.Background(), req, ReadRuntime{Executor: exec, Resolver: resolver, FallbackInstanceID: fallbackID})
}

func TestMonitorCurrentRequestHasNoRequiredFields(t *testing.T) {
	require.Nil(t, MonitorCurrentRequest{}.MissingFields())
}

// TestMonitorCurrentHandle_ResolvesTargetAndRendersFacts: a name reference
// resolves, the monitor call pins that UHostId, the reply renders the semantic
// metric label and the envelope is a monitor_query envelope scoped to the host.
func TestMonitorCurrentHandle_ResolvesTargetAndRendersFacts(t *testing.T) {
	exec := &fakeReadExec{result: monitorFixture("uhost-a")}
	resolver := refundResolver(t, [2]string{"uhost-a", "train-a"})

	result := runMonitorCurrent(t, exec, resolver, "", MonitorCurrentRequest{
		Targets: []platform.TargetRef{{Type: platform.TargetRefName, Value: "train-a"}},
	})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Equal(t, "GetCompShareInstanceMonitor", result.ToolAction)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, []string{"uhost-a"}, exec.calls[0].args["UHostIds"])
	assert.Contains(t, result.Reply, "CPU 使用率")
	assert.Contains(t, result.Reply, "12.5")
	require.NotNil(t, result.Envelope)
	assert.Equal(t, envelope.KindMonitorQuery, result.Envelope.Kind)
}

// TestMonitorCurrentHandle_FallbackInstanceID: an empty target set uses the
// session's selected instance instead of failing, matching the legacy handler.
func TestMonitorCurrentHandle_FallbackInstanceID(t *testing.T) {
	exec := &fakeReadExec{result: monitorFixture("uhost-a")}
	resolver := refundResolver(t, [2]string{"uhost-a", "train-a"})

	result := runMonitorCurrent(t, exec, resolver, "uhost-a", MonitorCurrentRequest{})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, []string{"uhost-a"}, exec.calls[0].args["UHostIds"])
}

// TestMonitorCurrentHandle_MissingTarget: no target and no session fallback is a
// missing-target fallback before any tool call.
func TestMonitorCurrentHandle_MissingTarget(t *testing.T) {
	exec := &fakeReadExec{}

	result := runMonitorCurrent(t, exec, nil, "", MonitorCurrentRequest{})

	require.Equal(t, platform.ReadStatusFallbackBeforeTool, result.Status)
	assert.Equal(t, platform.ReadFallbackMissingTarget, result.FallbackReason)
	assert.Empty(t, exec.calls)
}

func TestMonitorCurrentHandle_UnresolvedTarget(t *testing.T) {
	exec := &fakeReadExec{result: monitorFixture("uhost-a")}
	resolver := refundResolver(t, [2]string{"uhost-a", "train-a"})

	result := runMonitorCurrent(t, exec, resolver, "", MonitorCurrentRequest{
		Targets: []platform.TargetRef{{Type: platform.TargetRefName, Value: "ghost"}},
	})

	require.Equal(t, platform.ReadStatusFallbackBeforeTool, result.Status)
	assert.Equal(t, platform.ReadFallbackUnresolvedTarget, result.FallbackReason)
	assert.Empty(t, exec.calls)
}

// A cold exact ID is verified with a point Describe before monitoring. The
// envelope therefore carries an upstream-confirmed subject rather than a
// synthesized registry placeholder.
func TestMonitorCurrentHandle_ColdExactIDIsPointVerified(t *testing.T) {
	exec := &mapReadExec{results: map[string]map[string]any{
		resourceInfoAction: describeFixture(instanceRowMap("uhost-cold", "cold-host", "Running")),
		monitorAction:      monitorFixture("uhost-cold"),
	}}

	result := runMonitorCurrent(t, exec, coldRegistrySnapshot(), "", MonitorCurrentRequest{
		Targets: []platform.TargetRef{{Type: platform.TargetRefUHostIDUserInput, Value: "uhost-cold", Source: platform.SourceUserText}},
	})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	require.Len(t, exec.calls, 2)
	assert.Equal(t, resourceInfoAction, exec.calls[0].action)
	assert.Equal(t, monitorAction, exec.calls[1].action)
	require.NotNil(t, result.Envelope)
	require.Len(t, result.Envelope.Subjects, 1)
	assert.Equal(t, "uhost-cold", result.Envelope.Subjects[0].ID)
	assert.Equal(t, "cold-host", result.Envelope.Subjects[0].Name)
}

func TestMonitorCurrentHandle_ColdExactIDAbsentStopsBeforeMonitor(t *testing.T) {
	exec := &mapReadExec{results: map[string]map[string]any{
		resourceInfoAction: describeFixture(),
	}}
	result := runMonitorCurrent(t, exec, coldRegistrySnapshot(), "", MonitorCurrentRequest{
		Targets: []platform.TargetRef{{Type: platform.TargetRefUHostIDUserInput, Value: "uhost-missing", Source: platform.SourceUserText}},
	})
	require.Equal(t, platform.ReadStatusFallbackBeforeTool, result.Status)
	assert.Equal(t, platform.ReadFallbackUnresolvedTarget, result.FallbackReason)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, resourceInfoAction, exec.calls[0].action)
}

func TestMonitorCurrentHandle_UpstreamError(t *testing.T) {
	resolver := refundResolver(t, [2]string{"uhost-a", "train-a"})

	result := runMonitorCurrent(t, errReadExec{err: errors.New("boom")}, resolver, "", MonitorCurrentRequest{
		Targets: []platform.TargetRef{{Type: platform.TargetRefName, Value: "train-a"}},
	})

	require.Equal(t, platform.ReadStatusFailureAfterTool, result.Status)
	assert.Equal(t, platform.ReadFailureGenericRead, result.FailureClass)
	assert.Equal(t, "monitor_query"+": "+FriendlyReadFailureReply, result.Reply)
}

// monitorHistoryFixture is a recognized monitor payload with one multi-point CPU
// series per requested instance for the historical (aggregated) renderer.
func monitorHistoryFixture(uhostIDs ...string) map[string]any {
	instances := make([]any, 0, len(uhostIDs))
	for index, uhostID := range uhostIDs {
		base := float64(10 + index*10)
		instances = append(instances, map[string]any{
			"UHostId": uhostID,
			"Metrics": []any{
				map[string]any{
					"MetricKey": "uhost_cpu_used",
					"Results": []any{map[string]any{
						"Values": []any{
							map[string]any{"Timestamp": float64(1778173200), "Value": base},
							map[string]any{"Timestamp": float64(1778176800), "Value": base + 20},
						},
					}},
				},
			},
		})
	}
	return map[string]any{
		"Data": map[string]any{
			"List": instances,
		},
	}
}

func runMonitorHistory(t *testing.T, exec ReadExecutor, resolver EntityResolver, req MonitorHistoryRequest) ReadResult {
	t.Helper()
	reg := NewReadCapability(monitorHistoryReadSpec())
	return reg.Run(context.Background(), req, ReadRuntime{Executor: exec, Resolver: resolver})
}

func TestMonitorHistoryRequestRequiresTimeWindow(t *testing.T) {
	require.Equal(t, []platform.MissingField{{Name: "time_window", Reason: "required"}}, MonitorHistoryRequest{}.MissingFields())
	require.Nil(t, MonitorHistoryRequest{TimeWindow: &platform.TimeWindow{Type: platform.TimeWindowAbsolute, Start: "x"}}.MissingFields())
}

// TestMonitorHistoryHandle_ResolvesWindowAndCallsMonitor: a single resolved
// target plus an absolute window drives a monitor call carrying StartTime +
// EndTime, and the reply uses the aggregated (latest/avg/peak) render.
func TestMonitorHistoryHandle_ResolvesWindowAndCallsMonitor(t *testing.T) {
	exec := &fakeReadExec{result: monitorHistoryFixture("uhost-a")}
	resolver := refundResolver(t, [2]string{"uhost-a", "train-a"})

	result := runMonitorHistory(t, exec, resolver, MonitorHistoryRequest{
		Targets:    []platform.TargetRef{{Type: platform.TargetRefName, Value: "train-a"}},
		TimeWindow: &platform.TimeWindow{Type: platform.TimeWindowAbsolute, Start: "2026-05-08 01:00", End: "2026-05-08 02:00"},
	})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, []string{"uhost-a"}, exec.calls[0].args["UHostIds"])
	assert.Equal(t, int64(1778173200), exec.calls[0].args["StartTime"])
	assert.Equal(t, int64(1778176800), exec.calls[0].args["EndTime"])
	assert.Contains(t, result.Reply, "最新")
	assert.Contains(t, result.Reply, "平均")
	assert.Contains(t, result.Reply, "峰值")
}

// Explicit historical ranges remain intact for a batch upstream. The reply and
// structured envelope must keep the two instances distinguishable instead of
// merging same-named CPU series into one apparent host.
func TestMonitorHistoryHandle_MultiTargetPreservesSubjectsAndRangeAggregates(t *testing.T) {
	exec := &fakeReadExec{result: monitorHistoryFixture("uhost-a", "uhost-b")}
	resolver := refundResolver(t, [2]string{"uhost-a", "train-a"}, [2]string{"uhost-b", "train-b"})

	result := runMonitorHistory(t, exec, resolver, MonitorHistoryRequest{
		Targets: []platform.TargetRef{
			{Type: platform.TargetRefName, Value: "train-a"},
			{Type: platform.TargetRefName, Value: "train-b"},
		},
		TimeWindow: &platform.TimeWindow{Type: platform.TimeWindowAbsolute, Start: "2026-05-08 01:00", End: "2026-05-08 02:00"},
	})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, []string{"uhost-a", "uhost-b"}, exec.calls[0].args["UHostIds"])
	assert.Contains(t, result.Reply, "uhost-a · CPU 使用率")
	assert.Contains(t, result.Reply, "uhost-b · CPU 使用率")
	require.NotNil(t, result.Envelope)
	require.Len(t, result.Envelope.Subjects, 2)
	require.Len(t, result.Envelope.Facts, 6)
	for _, subjectID := range []string{"uhost-a", "uhost-b"} {
		aggregations := map[string]bool{}
		for _, fact := range result.Envelope.Facts {
			if fact.SubjectID == subjectID && fact.Key == "cpu_usage" {
				aggregations[fact.Aggregation] = true
				assert.Equal(t, "range", fact.Period)
			}
		}
		assert.Equal(t, map[string]bool{"latest": true, "average": true, "max": true}, aggregations)
	}
}

// TestMonitorHistoryHandle_InvalidWindowRejected: an unparseable window is a
// time-window fallback before any tool call.
func TestMonitorHistoryHandle_InvalidWindowRejected(t *testing.T) {
	exec := &fakeReadExec{result: monitorHistoryFixture("uhost-a")}
	resolver := refundResolver(t, [2]string{"uhost-a", "train-a"})

	result := runMonitorHistory(t, exec, resolver, MonitorHistoryRequest{
		Targets:    []platform.TargetRef{{Type: platform.TargetRefName, Value: "train-a"}},
		TimeWindow: &platform.TimeWindow{Type: platform.TimeWindowAbsolute, Start: "2026-06-21 10:00", End: "2026-06-21 09:00"},
	})

	require.Equal(t, platform.ReadStatusFallbackBeforeTool, result.Status)
	assert.Equal(t, platform.ReadFallbackTimeWindow, result.FallbackReason)
	assert.Empty(t, exec.calls)
}
