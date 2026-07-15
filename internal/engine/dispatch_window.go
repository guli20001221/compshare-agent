package engine

import (
	"sort"
	"strings"

	openai "github.com/sashabaranov/go-openai"

	"github.com/compshare-agent/internal/actionresolver"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/tools"
)

// centralAgentToolWindow is the grouped P6 capability surface. It intentionally
// does not expose the underlying API tools used by deterministic handlers, so
// every platform fact crosses ReadPlatformCapability and its EvidenceEnvelope.
// The two internal capabilities remain shadowed in the legacy registry until
// the grouped P6 rollout is complete; this function is the only opt-in view.
func centralAgentToolWindow(mutatingEnabled bool) []openai.Tool {
	registry := tools.DefaultCapabilityRegistry()
	var out []openai.Tool
	if capability, ok := registry.Lookup(tools.ReadPlatformCapabilityName); ok {
		out = append(out, readCapabilityToolFromRegistry(capability.Tool))
	}
	if capability, ok := registry.Lookup(tools.UpdateTaskStateName); ok {
		out = append(out, capability.Tool)
	}
	if mutatingEnabled {
		if capability, ok := registry.Lookup(tools.ProposeActionName); ok {
			out = append(out, proposalToolFromCatalog(capability.Tool))
		}
	}
	for _, capability := range registry.All() {
		if !capability.ExposedToAgent || capability.Tool.Function == nil {
			continue
		}
		if capability.Name == "SearchKnowledge" || capability.Policy.Route == tools.ActionRouteDiagnosis {
			out = append(out, capability.Tool)
		}
	}
	return out
}

func readCapabilityToolFromRegistry(base openai.Tool) openai.Tool {
	if base.Function == nil {
		return base
	}
	tool := base
	function := *base.Function
	root, ok := cloneSchemaObject(function.Parameters)
	if !ok {
		return base
	}
	properties, ok := root["properties"].(map[string]any)
	if !ok {
		return base
	}
	capabilities := []string{
		string(intent.IntentResourceInfo),
		string(intent.IntentMonitorQuery),
		string(intent.IntentMonitorHistory),
	}
	for _, capability := range intent.RoutingIntents() {
		capabilities = append(capabilities, string(capability))
	}
	properties["capability"] = map[string]any{"type": "string", "enum": capabilities}
	properties["slots"] = map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"target_refs": map[string]any{"type": "array", "items": map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{
					"type":        map[string]any{"type": "string", "enum": []string{"filter", "name", "uhost_id_user_input", "slot_position"}},
					"value":       map[string]any{"type": "string"},
					"source":      map[string]any{"type": "string", "enum": []string{"user_text", "prior_turn"}},
					"source_span": map[string]any{"type": "string"},
				},
				"required": []string{"type", "value", "source"},
			}},
			"metrics": map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []string{"cpu", "memory", "gpu", "vram"}}},
			"time_window": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
				"type":  map[string]any{"type": "string", "enum": []string{"preset", "relative", "absolute"}},
				"value": map[string]any{"type": "string"},
			}, "required": []string{"type", "value"}},
			"image_source": map[string]any{"type": "string", "enum": []string{"platform", "custom", "community", "shared"}},
			"search_query": map[string]any{"type": "string"},
			"list_mode":    map[string]any{"type": "string", "enum": []string{"all", "filtered"}},
			"price_kind":   map[string]any{"type": "string", "enum": []string{"account", "catalog"}},
			"cfs_kind":     map[string]any{"type": "string", "enum": []string{"list", "create_price", "upgrade_price", "refund"}},
			"size_gb":      map[string]any{"type": "integer"},
			"zone":         map[string]any{"type": "string"},
			"charge_type":  map[string]any{"type": "string"},
			"detail_level": map[string]any{"type": "string", "enum": []string{"summary", "full"}},
		},
	}
	function.Description = "执行平台只读能力。能力名称和参数结构来自服务端注册表；slots 只提交中心 Agent 从完整上下文中明确得到的参数，能力内部不会重新解释用户原话。"
	function.Parameters = root
	tool.Function = &function
	return tool
}

func proposalToolFromCatalog(base openai.Tool) openai.Tool {
	catalog, err := defaultActionCatalog()
	if err != nil || base.Function == nil {
		return base
	}
	tool := base
	function := *base.Function
	root, ok := cloneSchemaObject(function.Parameters)
	if !ok {
		return base
	}
	properties, ok := root["properties"].(map[string]any)
	if !ok {
		return base
	}
	operations := catalog.Operations()
	properties["operation"] = map[string]any{"type": "string", "enum": operations}
	fieldSet := map[string]struct{}{}
	var contracts []string
	for _, operation := range operations {
		spec, _ := catalog.Lookup(operation)
		fields := actionresolver.SortedFieldNames(spec)
		for _, field := range fields {
			fieldSet[field] = struct{}{}
		}
		var required []string
		for _, field := range fields {
			if spec.Fields[field].Required && spec.Fields[field].Codec != actionresolver.CodecSensitiveText {
				required = append(required, field)
			}
		}
		contracts = append(contracts, operation+"["+strings.Join(required, ",")+"]")
	}
	fieldNames := make([]string, 0, len(fieldSet))
	for field := range fieldSet {
		fieldNames = append(fieldNames, field)
	}
	sort.Strings(fieldNames)
	if slots, ok := properties["slots"].(map[string]any); ok {
		if items, ok := slots["items"].(map[string]any); ok {
			if slotProperties, ok := items["properties"].(map[string]any); ok {
				slotProperties["name"] = map[string]any{"type": "string", "enum": fieldNames}
			}
		}
	}
	function.Description += " 合法操作及非敏感必填字段由工作流注册表生成：" + strings.Join(contracts, "；") + "。敏感值由服务端密封通道注入，不得在 slots 中回显。"
	function.Parameters = root
	tool.Function = &function
	return tool
}

func cloneSchemaObject(value any) (map[string]any, bool) {
	source, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	cloned := make(map[string]any, len(source))
	for key, item := range source {
		switch typed := item.(type) {
		case map[string]any:
			copy, _ := cloneSchemaObject(typed)
			cloned[key] = copy
		case []string:
			cloned[key] = append([]string(nil), typed...)
		case []any:
			cloned[key] = append([]any(nil), typed...)
		default:
			cloned[key] = item
		}
	}
	return cloned, true
}

func centralAgentToolNames(mutatingEnabled bool) []string {
	window := centralAgentToolWindow(mutatingEnabled)
	names := make([]string, 0, len(window))
	for _, tool := range window {
		if tool.Function != nil && tool.Function.Name != "" {
			names = append(names, tool.Function.Name)
		}
	}
	return names
}
