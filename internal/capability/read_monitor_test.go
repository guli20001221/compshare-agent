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

func TestMonitorCurrentHandle_UpstreamError(t *testing.T) {
	resolver := refundResolver(t, [2]string{"uhost-a", "train-a"})

	result := runMonitorCurrent(t, errReadExec{err: errors.New("boom")}, resolver, "", MonitorCurrentRequest{
		Targets: []platform.TargetRef{{Type: platform.TargetRefName, Value: "train-a"}},
	})

	require.Equal(t, platform.ReadStatusFailureAfterTool, result.Status)
	assert.Equal(t, platform.ReadFailureGenericRead, result.FailureClass)
	assert.Equal(t, "monitor_query"+": "+FriendlyReadFailureReply, result.Reply)
}

// monitorHistoryFixture is a recognized monitor payload with a multi-point CPU
// series for the historical (aggregated) renderer.
func monitorHistoryFixture(uhostID string) map[string]any {
	return map[string]any{
		"Data": map[string]any{
			"List": []any{
				map[string]any{
					"UHostId": uhostID,
					"Metrics": []any{
						map[string]any{
							"MetricKey": "uhost_cpu_used",
							"Results": []any{map[string]any{
								"Values": []any{
									map[string]any{"Timestamp": float64(1778173200), "Value": float64(10)},
									map[string]any{"Timestamp": float64(1778176800), "Value": float64(30)},
								},
							}},
						},
					},
				},
			},
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
	require.Nil(t, MonitorHistoryRequest{TimeWindow: &platform.TimeWindow{Type: platform.TimeWindowAbsolute, Value: "x"}}.MissingFields())
}

// TestMonitorHistoryHandle_ResolvesWindowAndCallsMonitor: a single resolved
// target plus an absolute window drives a monitor call carrying StartTime +
// EndTime, and the reply uses the aggregated (latest/avg/peak) render.
func TestMonitorHistoryHandle_ResolvesWindowAndCallsMonitor(t *testing.T) {
	exec := &fakeReadExec{result: monitorHistoryFixture("uhost-a")}
	resolver := refundResolver(t, [2]string{"uhost-a", "train-a"})

	result := runMonitorHistory(t, exec, resolver, MonitorHistoryRequest{
		Targets:    []platform.TargetRef{{Type: platform.TargetRefName, Value: "train-a"}},
		TimeWindow: &platform.TimeWindow{Type: platform.TimeWindowAbsolute, Value: "2026-05-08 01:00 到 02:00"},
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

// TestMonitorHistoryHandle_MultiTargetRejected: historical monitoring is single
// instance; two resolved targets is a validation fallback before any tool call.
func TestMonitorHistoryHandle_MultiTargetRejected(t *testing.T) {
	exec := &fakeReadExec{result: monitorHistoryFixture("uhost-a")}
	resolver := refundResolver(t, [2]string{"uhost-a", "train-a"}, [2]string{"uhost-b", "train-b"})

	result := runMonitorHistory(t, exec, resolver, MonitorHistoryRequest{
		Targets: []platform.TargetRef{
			{Type: platform.TargetRefName, Value: "train-a"},
			{Type: platform.TargetRefName, Value: "train-b"},
		},
		TimeWindow: &platform.TimeWindow{Type: platform.TimeWindowAbsolute, Value: "2026-05-08 01:00 到 02:00"},
	})

	require.Equal(t, platform.ReadStatusFallbackBeforeTool, result.Status)
	assert.Equal(t, platform.ReadFallbackValidation, result.FallbackReason)
	assert.Empty(t, exec.calls)
}

// TestMonitorHistoryHandle_InvalidWindowRejected: an unparseable window is a
// time-window fallback before any tool call.
func TestMonitorHistoryHandle_InvalidWindowRejected(t *testing.T) {
	exec := &fakeReadExec{result: monitorHistoryFixture("uhost-a")}
	resolver := refundResolver(t, [2]string{"uhost-a", "train-a"})

	result := runMonitorHistory(t, exec, resolver, MonitorHistoryRequest{
		Targets:    []platform.TargetRef{{Type: platform.TargetRefName, Value: "train-a"}},
		TimeWindow: &platform.TimeWindow{Type: platform.TimeWindowAbsolute, Value: "2026-06-21 10:00 到 09:00"},
	})

	require.Equal(t, platform.ReadStatusFallbackBeforeTool, result.Status)
	assert.Equal(t, platform.ReadFallbackTimeWindow, result.FallbackReason)
	assert.Empty(t, exec.calls)
}
