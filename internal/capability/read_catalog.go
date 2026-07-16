package capability

import (
	"fmt"
	"reflect"
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
	// RequestType is the concrete request struct this capability decodes into.
	// The catalog consistency test reflects it against the tool schema so a field
	// added to the struct without the schema (or vice versa) fails loudly.
	RequestType reflect.Type
	decode      func(map[string]any) (ReadRequest, error)
	// migrated is non-nil for capabilities that own a typed vertical
	// (ReadCapabilitySpec). The engine dispatches these through the typed kernel
	// instead of the legacy intent.Slots handler. nil = not yet migrated.
	migrated *RegisteredRead
}

func ReadToolName(id intent.Intent) string { return ReadToolPrefix + string(id) }

func namedReadToolName(name string) string { return ReadToolPrefix + name }

func ReadDefinitions() []ReadDefinition {
	definitions := []ReadDefinition{
		migratedReadDefinition(intent.IntentResourceInfo, NewReadCapability(resourceReadSpec())),
		migratedReadDefinition(intent.IntentMonitorQuery, NewReadCapability(monitorCurrentReadSpec())),
		migratedReadDefinition(intent.IntentMonitorHistory, NewReadCapability(monitorHistoryReadSpec())),
		migratedReadDefinition(intent.IntentGPUSpecsQuery, NewReadCapability(gpuSpecsReadSpec())),
		newReadDefinition[StockAvailabilityRequest](string(intent.IntentStockAvailability), intent.IntentStockAvailability, "查询 GPU 机型的实时可售性。", objectSchema(map[string]any{"gpu_type": stringSchema(), "zone": stringSchema()}, nil)),
		newReadDefinition[ImageListRequest](string(intent.IntentImageList), intent.IntentImageList, "查询平台、自制、社区或共享镜像。", objectSchema(map[string]any{"source": enumSchema("platform", "custom", "community", "shared"), "query": stringSchema(), "mode": enumSchema("all", "filtered")}, nil)),
		migratedReadDefinition(intent.IntentImageTagCatalog, NewReadCapability(imageTagCatalogReadSpec())),
		migratedReadDefinition(intent.IntentModelRepositoryBrowse, NewReadCapability(modelRepositoryReadSpec())),
		migratedReadDefinition(intent.IntentNetAcceleratorStatus, NewReadCapability(netAcceleratorReadSpec())),
		migratedReadDefinition(intent.IntentPricingQuery, NewReadCapability(pricingReadSpec())),
		migratedReadDefinition(intent.IntentRefundEstimate, NewReadCapability(refundReadSpec())),
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
		RequestType: reflect.TypeOf(*new(T)),
		decode:      func(args map[string]any) (ReadRequest, error) { return decodeStrictRead[T](args) },
	}
}

// migratedReadDefinition wraps a typed ReadCapabilitySpec as a catalog entry.
// Its tool schema and decoder come from the spec; the engine dispatches it
// through the typed kernel (RegisteredRead.Run) rather than the legacy handler.
func migratedReadDefinition(readIntent intent.Intent, reg RegisteredRead) ReadDefinition {
	registered := reg
	return ReadDefinition{
		Name: reg.Label, Intent: readIntent, Description: reg.Description, Tool: reg.Tool,
		RequestType: reg.RequestType(),
		decode:      func(args map[string]any) (ReadRequest, error) { return registered.Decode(args) },
		migrated:    &registered,
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

// MigratedRead returns the typed vertical for a read tool, or false when the
// capability still runs through the legacy intent.Slots handler.
func MigratedRead(toolName string) (RegisteredRead, bool) {
	for _, definition := range ReadDefinitions() {
		if definition.Tool.Function != nil && definition.Tool.Function.Name == toolName && definition.migrated != nil {
			return *definition.migrated, true
		}
	}
	return RegisteredRead{}, false
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
