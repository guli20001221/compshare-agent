package capability

import (
	"errors"
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

func TestCurrentTurnInstanceIDMustRemainWhole(t *testing.T) {
	const fullID = "uhost-1ug1k5sxldb2"
	userText := "帮我查询实例 " + fullID

	require.NoError(t, ValidateCurrentTurnGrounding(ResourceInfoRequest{Targets: []platform.TargetRef{{
		Type: platform.TargetRefUHostIDUserInput, Value: fullID, Source: platform.SourceUserText,
	}}}, userText))
	require.Error(t, ValidateCurrentTurnGrounding(ResourceInfoRequest{Targets: []platform.TargetRef{{
		Type: platform.TargetRefUHostIDUserInput, Value: "uhost-1ug1k5sxldb", Source: platform.SourcePriorTurn,
	}}}, userText))
	require.NoError(t, ValidateCurrentTurnGrounding(ResourceInfoRequest{Targets: []platform.TargetRef{{
		Type: platform.TargetRefUHostIDUserInput, Value: fullID, Source: platform.SourceUserText,
	}}}, "帮我查询实例 UHOST-1UG1K5SXLDB2"), "instance IDs are case-insensitive upstream identifiers")
	require.NoError(t, ValidateCurrentTurnGrounding(ResourceInfoRequest{Targets: []platform.TargetRef{{
		Type: platform.TargetRefUHostIDUserInput, Value: fullID, Source: platform.SourcePriorTurn,
	}}}, "继续查它", "上轮我说的是 "+fullID))
	require.Error(t, ValidateCurrentTurnGrounding(ResourceInfoRequest{Targets: []platform.TargetRef{{
		Type: platform.TargetRefUHostIDUserInput, Value: "uhost-1ug1k5sxldb", Source: platform.SourcePriorTurn,
	}}}, "继续查它", "上轮我说的是 "+fullID))
	require.NoError(t, ValidateCurrentTurnGrounding(ResourceInfoRequest{Targets: []platform.TargetRef{
		{Type: platform.TargetRefUHostIDUserInput, Value: fullID, Source: platform.SourceUserText},
		{Type: platform.TargetRefUHostIDUserInput, Value: "uhost-previous123", Source: platform.SourcePriorTurn},
	}}, "对比 "+fullID+" 和刚才那台", "上一轮是 uhost-previous123"))
	require.NoError(t, ValidateCurrentTurnGrounding(ResourceInfoRequest{Targets: []platform.TargetRef{{
		Type: platform.TargetRefUHostIDUserInput, Value: "uhost-from-prior-turn", Source: platform.SourcePriorTurn,
	}}}, "继续查它"), "without any literal ID, a normal carried-target follow-up remains valid")
}

func TestInstanceIDGroundingMismatchCarriesCompleteUserLiteralsWithoutSelectingOne(t *testing.T) {
	err := ValidateUserLiteralInstanceID(
		"uhost-current-12",
		"对比 uhost-current-123 和 UHOST-OTHER-456",
		"上一轮是 uhost-prior-789 和 uhost-current-123",
	)
	var mismatch *InstanceIDGroundingMismatch
	require.True(t, errors.As(err, &mismatch), err)
	require.Equal(t, "uhost-current-12", mismatch.Provided)
	require.Equal(t, []string{"uhost-current-123", "UHOST-OTHER-456", "uhost-prior-789"}, mismatch.UserLiteralIDs,
		"the boundary reports user-authored evidence in recency order and does not pick a target")

	require.NoError(t, ValidateUserLiteralInstanceID(
		"uhost-current-123", "查询 UHOST-CURRENT-123",
	), "case-only normalization is not truncation")
}
