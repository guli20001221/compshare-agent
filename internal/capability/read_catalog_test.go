package capability

import (
	"testing"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/routing"
	"github.com/stretchr/testify/require"
)

func TestReadDefinitionsUseCapabilitySpecificSchemas(t *testing.T) {
	definitions := ReadDefinitions()
	byTool := make(map[string]ReadDefinition, len(definitions))
	coveredIntents := map[intent.Intent]int{}
	for _, definition := range definitions {
		require.NotNil(t, definition.Tool.Function)
		name := definition.Tool.Function.Name
		_, duplicate := byTool[name]
		require.False(t, duplicate, "duplicate read capability %s", name)
		byTool[name] = definition
		coveredIntents[definition.Intent]++
		parsed, ok := ReadIntentForTool(name)
		require.True(t, ok)
		require.Equal(t, definition.Intent, parsed)
		root, ok := definition.Tool.Function.Parameters.(map[string]any)
		require.True(t, ok)
		properties, _ := root["properties"].(map[string]any)
		require.NotContains(t, properties, "slots", "通用槽位袋不得重新进入模型协议")
	}
	for _, route := range routing.GeneratedRoutes() {
		require.Greater(t, coveredIntents[intent.Intent(route.IntentLabel)], 0, "route %s is missing from agent read catalog", route.Name)
	}
	require.Equal(t, 4, coveredIntents[intent.IntentCFSInfo], "CFS 查询、创建询价、扩容询价和退费估算必须是独立能力")
}

func TestReadRequestDecodingIsStrictAndTyped(t *testing.T) {
	pricingTool := ReadToolName(intent.IntentPricingQuery)
	_, request, err := DecodeReadRequest(pricingTool, map[string]any{"gpu_type": "4090", "gpu_count": 8, "price_kind": "account"})
	require.NoError(t, err)
	require.Equal(t, PricingRequest{GPUType: "4090", GPUCount: 8, Kind: intent.PriceKindAccount}, request)

	_, _, err = DecodeReadRequest(pricingTool, map[string]any{"search_query": "4090"})
	require.Error(t, err, "旧通用槽位字段必须被拒绝")
}

func TestReadDefinitionsExposeConcreteImageAndPriceTools(t *testing.T) {
	names := make([]string, 0, len(ReadDefinitions()))
	for _, definition := range ReadDefinitions() {
		names = append(names, definition.Tool.Function.Name)
	}
	require.Contains(t, names, ReadToolName(intent.IntentImageList))
	require.Contains(t, names, ReadToolName(intent.IntentPricingQuery))
	require.Contains(t, names, ReadToolName(intent.IntentGPUSpecsQuery))
	require.Contains(t, names, namedReadToolName(readCFSUpgradePrice))
}
