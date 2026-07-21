package capability

import (
	"testing"

	"github.com/compshare-agent/internal/platform"
	"github.com/stretchr/testify/require"
)

func TestStockZoneMentionsMustComeFromCurrentTurn(t *testing.T) {
	request := StockAvailabilityRequest{GPUType: "4090", ZoneMentions: []string{"华北一C", "华北二C"}}
	require.NoError(t, ValidateCurrentTurnGrounding(request, "分别查询华北一C和华北二C的4090库存"))

	request.ZoneMentions = []string{"cn-bj2-03", "cn-wlcb-01"}
	require.Error(t, ValidateCurrentTurnGrounding(request, "分别查询华北一C和华北二C的4090库存"),
		"the model may not silently replace a named zone with a different canonical id")
}

func TestMonitorYesterdayCannotBecomeInventedAbsoluteDates(t *testing.T) {
	invented := MonitorHistoryRequest{TimeWindow: &platform.TimeWindow{
		Type: platform.TimeWindowAbsolute, Start: "2026-07-18 00:00", End: "2026-07-19 00:00", SourceSpan: "昨天",
	}}
	require.Error(t, ValidateCurrentTurnGrounding(invented, "查询昨天的CPU历史监控"))

	preset := MonitorHistoryRequest{TimeWindow: &platform.TimeWindow{
		Type: platform.TimeWindowPreset, Preset: "yesterday", SourceSpan: "昨天",
	}}
	require.NoError(t, ValidateCurrentTurnGrounding(preset, "查询昨天的CPU历史监控"))
}

func TestMonitorExplicitAbsoluteWindowIsGrounded(t *testing.T) {
	request := MonitorHistoryRequest{TimeWindow: &platform.TimeWindow{
		Type: platform.TimeWindowAbsolute, Start: "2026-07-20 01:00", End: "2026-07-20 02:00",
		SourceSpan: "2026-07-20 01:00 到 02:00",
	}}
	require.NoError(t, ValidateCurrentTurnGrounding(request, "查询 2026-07-20 01:00 到 02:00 的CPU监控"))
}
