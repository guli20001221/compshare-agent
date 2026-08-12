package engine

import (
	"strings"

	"github.com/compshare-agent/internal/capability"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/platform"
	openai "github.com/sashabaranov/go-openai"
)

// feishuPublicPlatformReadTools is intentionally a positive allowlist. A newly
// added read capability is NOT safe for an external group merely because it is
// non-mutating: it may still disclose account, instance, pricing, or shared
// artifact data. Classify each new capability here deliberately.
var feishuPublicPlatformReadTools = map[string]bool{
	capability.ReadToolName(intent.IntentGPUSpecsQuery):         true,
	capability.ReadToolName(intent.IntentStockAvailability):     true,
	capability.ReadToolName(intent.IntentImageList):             true,
	capability.ReadToolName(intent.IntentZoneCatalog):           true,
	capability.ReadToolName(intent.IntentModelRepositoryBrowse): true,
	capability.ReadToolName(intent.IntentPricingQuery):          true,
}

const publicPlatformReadOnlyBoundary = "当前外部群仅允许查询公开平台目录，不能查询账号或实例数据、执行诊断或发起操作"

// centralAgentPublicPlatformReadOnlyToolWindow is the external-group surface:
// knowledge retrieval plus every current publicly safe platform query. It does
// not include account-scoped read tools (instances, monitors, custom/shared
// images, image tags, account prices, CFS, accelerator state), because the
// Feishu sender is not an authenticated Compshare user.
func centralAgentPublicPlatformReadOnlyToolWindow() []openai.Tool {
	out := append([]openai.Tool(nil), centralAgentKnowledgeToolWindow()...)
	for _, definition := range capability.ReadDefinitions() {
		if definition.Tool.Function == nil || !feishuPublicPlatformReadTools[definition.Tool.Function.Name] {
			continue
		}
		tool, ok := publicPlatformReadOnlyTool(definition.Tool)
		if !ok {
			// A malformed future schema must remove the capability from this
			// external surface instead of accidentally advertising its wider,
			// console-only variant.
			continue
		}
		out = append(out, tool)
	}
	return out
}

// publicPlatformReadOnlyTool narrows schemas whose general-console variants
// include account-scoped options. The execution guard below repeats the same
// restrictions because an LLM may send a tool call outside the advertised
// schema.
func publicPlatformReadOnlyTool(tool openai.Tool) (openai.Tool, bool) {
	if tool.Function == nil {
		return openai.Tool{}, false
	}
	function := *tool.Function
	if function.Name != capability.ReadToolName(intent.IntentImageList) &&
		function.Name != capability.ReadToolName(intent.IntentPricingQuery) {
		return tool, true
	}
	parameters, ok := cloneSchemaObject(function.Parameters)
	if !ok {
		return openai.Tool{}, false
	}
	properties, _ := parameters["properties"].(map[string]any)
	switch function.Name {
	case capability.ReadToolName(intent.IntentImageList):
		if source, ok := properties["source"].(map[string]any); ok {
			source["enum"] = []string{string(platform.ImageSourcePlatform), string(platform.ImageSourceCommunity)}
		}
		function.Description = strings.TrimSpace(function.Description + " 外部群仅可查询平台或社区镜像目录，不能查询自制或共享镜像。")
	case capability.ReadToolName(intent.IntentPricingQuery):
		if kind, ok := properties["price_kind"].(map[string]any); ok {
			kind["enum"] = []string{string(platform.PriceKindCatalog)}
		}
		function.Description = strings.TrimSpace(function.Description + " 外部群只能返回公开目录价，不能查询账号折扣或实付价。")
	}
	function.Parameters = parameters
	tool.Function = &function
	return tool, true
}

func publicPlatformReadOnlyToolAllowed(action string) bool {
	if knowledgeOnlyToolAllowed(action) {
		return true
	}
	return feishuPublicPlatformReadTools[action]
}

// publicPlatformReadOnlyArgsAllowed validates the few option-bearing tools
// after JSON decode and before any handler runs. It also turns an omitted price
// kind into catalog, since the general console capability defaults it to an
// account-specific price.
func publicPlatformReadOnlyArgsAllowed(action string, args map[string]any) bool {
	switch action {
	case capability.ReadToolName(intent.IntentImageList):
		raw, exists := args["source"]
		if !exists {
			return true // imageListHandle's existing default is platform.
		}
		source, ok := raw.(string)
		if !ok {
			return false
		}
		switch platform.ImageSource(strings.ToLower(strings.TrimSpace(source))) {
		case "", platform.ImageSourcePlatform, platform.ImageSourceCommunity:
			return true
		default:
			return false
		}
	case capability.ReadToolName(intent.IntentPricingQuery):
		raw, exists := args["price_kind"]
		if exists {
			kind, ok := raw.(string)
			if !ok || (strings.TrimSpace(kind) != "" && platform.PriceKind(strings.TrimSpace(kind)) != platform.PriceKindCatalog) {
				return false
			}
		}
		args["price_kind"] = string(platform.PriceKindCatalog)
	}
	return true
}
