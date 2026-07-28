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

func TestImageListQueryMustBeGroundedInCurrentTurnPurpose(t *testing.T) {
	userText := "为我推荐一个做数字人的镜像"

	require.NoError(t, ValidateCurrentTurnGrounding(ImageListRequest{
		Source: platform.ImageSourceCommunity, Query: "数字人", Mode: platform.ListModeFiltered,
	}, userText))
	require.NoError(t, ValidateCurrentTurnGrounding(ImageListRequest{
		Source: platform.ImageSourceCommunity, Mode: platform.ListModeAll,
	}, userText), "an unfiltered browse does not need a query source span")

	require.Error(t, ValidateCurrentTurnGrounding(ImageListRequest{
		Source: platform.ImageSourceCommunity, Query: "LiveTalking", Mode: platform.ListModeFiltered,
	}, userText), "the Agent may not guess a candidate name and use it to narrow the catalog")
}

func TestImageListSemanticExpansionKeepsTheGroundedBaseline(t *testing.T) {
	require.NoError(t, ValidateCurrentTurnGrounding(ImageListRequest{
		Source: platform.ImageSourceCommunity, Query: "大模型推理",
		SemanticQueries: []string{"vllm", "sglang"}, Mode: platform.ListModeFiltered,
	}, "推荐一个大模型推理镜像"))
	require.NoError(t, ValidateCurrentTurnGrounding(ImageListRequest{
		Source: platform.ImageSourceCommunity, Query: "AI绘画",
		SemanticQueries: []string{"ComfyUI"}, Mode: platform.ListModeFiltered,
	}, "找一个AI绘画镜像"))

	require.Error(t, ValidateCurrentTurnGrounding(ImageListRequest{
		Source:          platform.ImageSourceCommunity,
		SemanticQueries: []string{"LiveTalking"}, Mode: platform.ListModeFiltered,
	}, "推荐数字人镜像"), "an expansion cannot replace the grounded user query")
	require.Error(t, ValidateCurrentTurnGrounding(ImageListRequest{
		Source: platform.ImageSourcePlatform, Query: "AI绘画",
		SemanticQueries: []string{"ComfyUI"}, Mode: platform.ListModeFiltered,
	}, "找一个AI绘画镜像"), "semantic expansions are implemented only for the community catalog")
	require.Error(t, ValidateCurrentTurnGrounding(ImageListRequest{
		Source: platform.ImageSourceCommunity, Query: "AI绘画",
		SemanticQueries: []string{"a", "b", "c", "d"}, Mode: platform.ListModeFiltered,
	}, "找一个AI绘画镜像"), "the expansion fan-out is bounded")
}

func TestImageListQueryMayReuseARecentUserPhraseButNotAssistantProse(t *testing.T) {
	request := ImageListRequest{
		Source: platform.ImageSourceCommunity, Query: "InfiniteTalk", Mode: platform.ListModeFiltered,
	}
	require.NoError(t, ValidateCurrentTurnGrounding(
		request, "那个还有别的版本吗", "上轮我问的是 InfiniteTalk",
	))
	require.Error(t, ValidateCurrentTurnGrounding(
		request, "那个还有别的版本吗",
	), "assistant or tool text is intentionally not an evidence source")
}

func TestZoneCatalogQueryMustComeFromCurrentTurn(t *testing.T) {
	require.NoError(t, ValidateCurrentTurnGrounding(ZoneCatalogRequest{Query: "华北一 C"}, "华北一C对应哪个 Zone？"))
	require.NoError(t, ValidateCurrentTurnGrounding(ZoneCatalogRequest{}, "平台有哪些可用区？"))
	require.Error(t, ValidateCurrentTurnGrounding(ZoneCatalogRequest{Query: "上海二B"}, "华北一C对应哪个 Zone？"))
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
