package capability

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/compshare-agent/internal/intent"
	openai "github.com/sashabaranov/go-openai"
)

const ReadToolPrefix = "ReadCapability_"

const (
	readCFSList           = "cfs_list"
	readCFSCreatePrice    = "cfs_create_price"
	readCFSUpgradePrice   = "cfs_upgrade_price"
	readCFSRefundEstimate = "cfs_refund_estimate"
)

type ReadDefinition struct {
	Name        string
	Intent      intent.Intent
	Description string
	Tool        openai.Tool
	decode      func(map[string]any) (ReadRequest, error)
}

func ReadToolName(id intent.Intent) string { return ReadToolPrefix + string(id) }

func namedReadToolName(name string) string { return ReadToolPrefix + name }

func ReadDefinitions() []ReadDefinition {
	definitions := []ReadDefinition{
		newReadDefinition[ResourceInfoRequest](string(intent.IntentResourceInfo), intent.IntentResourceInfo, "查询当前账号的实例列表、实例状态和实例配置。", objectSchema(map[string]any{"targets": targetRefsSchema()}, nil)),
		newReadDefinition[MonitorCurrentRequest](string(intent.IntentMonitorQuery), intent.IntentMonitorQuery, "查询实例当前 CPU、内存、GPU 或显存监控数据。", objectSchema(map[string]any{"targets": targetRefsSchema(), "metrics": metricsSchema()}, nil)),
		newReadDefinition[MonitorHistoryRequest](string(intent.IntentMonitorHistory), intent.IntentMonitorHistory, "查询实例在明确时间范围内的历史监控数据。", objectSchema(map[string]any{"targets": targetRefsSchema(), "metrics": metricsSchema(), "time_window": timeWindowSchema()}, []string{"time_window"})),
		newReadDefinition[GPUSpecsRequest](string(intent.IntentGPUSpecsQuery), intent.IntentGPUSpecsQuery, "查询 GPU 机型的静态规格。", objectSchema(map[string]any{"gpu_type": stringSchema(), "detail_level": enumSchema("summary", "full")}, nil)),
		newReadDefinition[StockAvailabilityRequest](string(intent.IntentStockAvailability), intent.IntentStockAvailability, "查询 GPU 机型的实时可售性。", objectSchema(map[string]any{"gpu_type": stringSchema(), "zone": stringSchema()}, nil)),
		newReadDefinition[ImageListRequest](string(intent.IntentImageList), intent.IntentImageList, "查询平台、自制、社区或共享镜像。", objectSchema(map[string]any{"source": enumSchema("platform", "custom", "community", "shared"), "query": stringSchema(), "mode": enumSchema("all", "filtered")}, nil)),
		newReadDefinition[ImageTagCatalogRequest](string(intent.IntentImageTagCatalog), intent.IntentImageTagCatalog, "查询平台镜像标签和分类。", objectSchema(nil, nil)),
		newReadDefinition[ModelRepositoryRequest](string(intent.IntentModelRepositoryBrowse), intent.IntentModelRepositoryBrowse, "浏览公共模型仓库。", objectSchema(map[string]any{"query": stringSchema(), "mode": enumSchema("all", "filtered")}, nil)),
		newReadDefinition[NetworkAcceleratorStatusRequest](string(intent.IntentNetAcceleratorStatus), intent.IntentNetAcceleratorStatus, "查询网络加速状态。", objectSchema(map[string]any{"targets": targetRefsSchema()}, nil)),
		newReadDefinition[PricingRequest](string(intent.IntentPricingQuery), intent.IntentPricingQuery, "查询指定 GPU 机型的账号价或目录价。", objectSchema(map[string]any{"gpu_type": stringSchema(), "gpu_count": map[string]any{"type": "integer", "minimum": 1}, "price_kind": enumSchema("account", "catalog")}, []string{"gpu_type"})),
		newReadDefinition[RefundEstimateRequest](string(intent.IntentRefundEstimate), intent.IntentRefundEstimate, "估算指定实例当前可退金额，不执行释放。", objectSchema(map[string]any{"targets": targetRefsSchema()}, []string{"targets"})),
		newReadDefinition[CFSListRequest](readCFSList, intent.IntentCFSInfo, "查询 CFS 列表或指定 CFS 状态。", objectSchema(map[string]any{"cfs": cfsRefSchema()}, nil)),
		newReadDefinition[CFSCreatePriceRequest](readCFSCreatePrice, intent.IntentCFSInfo, "估算创建 CFS 的价格。", objectSchema(map[string]any{"zone": stringSchema(), "target_size_gb": positiveIntegerSchema(), "charge_type": stringSchema()}, []string{"zone", "target_size_gb"})),
		newReadDefinition[CFSUpgradePriceRequest](readCFSUpgradePrice, intent.IntentCFSInfo, "估算指定 CFS 扩容到目标容量的价格。", objectSchema(map[string]any{"cfs": cfsRefSchema(), "target_size_gb": positiveIntegerSchema()}, []string{"cfs", "target_size_gb"})),
		newReadDefinition[CFSRefundEstimateRequest](readCFSRefundEstimate, intent.IntentCFSInfo, "估算指定 CFS 当前可退金额。", objectSchema(map[string]any{"cfs": cfsRefSchema()}, []string{"cfs"})),
	}
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].Name < definitions[j].Name })
	return definitions
}

func newReadDefinition[T ReadRequest](name string, readIntent intent.Intent, description string, parameters map[string]any) ReadDefinition {
	toolName := namedReadToolName(name)
	return ReadDefinition{
		Name: name, Intent: readIntent, Description: description,
		Tool: openai.Tool{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{
			Name: toolName, Description: strings.TrimSpace(description), Parameters: parameters,
		}},
		decode: func(args map[string]any) (ReadRequest, error) { return decodeStrictRequest[T](args) },
	}
}

func DecodeReadRequest(toolName string, args map[string]any) (intent.Intent, ReadRequest, error) {
	for _, definition := range ReadDefinitions() {
		if definition.Tool.Function != nil && definition.Tool.Function.Name == toolName {
			request, err := definition.decode(args)
			return definition.Intent, request, err
		}
	}
	return "", nil, fmt.Errorf("unknown read capability %q", toolName)
}

func ReadIntentForTool(name string) (intent.Intent, bool) {
	for _, definition := range ReadDefinitions() {
		if definition.Tool.Function != nil && definition.Tool.Function.Name == name {
			return definition.Intent, true
		}
	}
	return "", false
}

func decodeStrictRequest[T ReadRequest](args map[string]any) (ReadRequest, error) {
	payload, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var request T
	if err := decoder.Decode(&request); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("trailing JSON value")
	}
	return request, nil
}

func objectSchema(properties map[string]any, required []string) map[string]any {
	if properties == nil {
		properties = map[string]any{}
	}
	schema := map[string]any{"type": "object", "additionalProperties": false, "properties": properties}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func stringSchema() map[string]any { return map[string]any{"type": "string"} }
func enumSchema(values ...string) map[string]any {
	return map[string]any{"type": "string", "enum": values}
}
func positiveIntegerSchema() map[string]any { return map[string]any{"type": "integer", "minimum": 1} }

func targetRefSchema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"type":        map[string]any{"type": "string", "enum": []string{"filter", "name", "uhost_id_user_input", "slot_position"}},
			"value":       map[string]any{"type": "string"},
			"source":      map[string]any{"type": "string", "enum": []string{"user_text", "prior_turn"}},
			"source_span": map[string]any{"type": "string"},
		},
		"required": []string{"type", "value", "source"},
	}
}

func targetRefsSchema() map[string]any {
	return map[string]any{"type": "array", "items": targetRefSchema()}
}
func cfsRefSchema() map[string]any {
	return objectSchema(map[string]any{"id": stringSchema()}, []string{"id"})
}
func metricsSchema() map[string]any {
	return map[string]any{"type": "array", "items": enumSchema("cpu", "memory", "gpu", "vram")}
}
func timeWindowSchema() map[string]any {
	return objectSchema(map[string]any{"type": enumSchema("preset", "relative", "absolute"), "value": stringSchema()}, []string{"type", "value"})
}
