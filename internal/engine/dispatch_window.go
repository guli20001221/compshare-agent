package engine

import (
	"sort"
	"strings"

	openai "github.com/sashabaranov/go-openai"

	"github.com/compshare-agent/internal/actionresolver"
	"github.com/compshare-agent/internal/capability"
	"github.com/compshare-agent/internal/tools"
)

// centralAgentToolWindow is the grouped P6 capability surface. It intentionally
// does not expose the underlying API tools used by deterministic handlers. Each
// high-level read is a distinct, catalog-generated tool, while every platform
// fact still crosses the same EvidenceEnvelope adapter.
func centralAgentToolWindow(mutatingEnabled bool) []openai.Tool {
	registry := tools.DefaultCapabilityRegistry()
	var out []openai.Tool
	if capability, ok := registry.Lookup(tools.UpdateTaskStateName); ok {
		out = append(out, capability.Tool)
	}
	if mutatingEnabled {
		if capability, ok := registry.Lookup(tools.ProposeActionName); ok {
			out = append(out, proposalToolsFromCatalog(capability.Tool)...)
		}
	}
	for _, definition := range capability.ReadDefinitions() {
		out = append(out, definition.Tool)
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

const proposalToolPrefix = "ProposeAction_"

func proposalToolName(operation string) string { return proposalToolPrefix + operation }

func proposalOperationForTool(name string) (string, bool) {
	operation, ok := strings.CutPrefix(name, proposalToolPrefix)
	if !ok {
		return "", false
	}
	catalog, err := defaultActionCatalog()
	if err != nil {
		return "", false
	}
	_, ok = catalog.Lookup(operation)
	return operation, ok
}

// proposalToolsFromCatalog gives every write operation one discoverable,
// generated proposal capability. The model selects the operation by choosing a
// tool; the server injects the canonical operation ID and still sends every
// candidate through Resolver, Gate, confirmation and journal.
func proposalToolsFromCatalog(base openai.Tool) []openai.Tool {
	catalog, err := defaultActionCatalog()
	if err != nil || base.Function == nil {
		return nil
	}
	var out []openai.Tool
	for _, operation := range catalog.Operations() {
		spec, _ := catalog.Lookup(operation)
		tool := proposalToolForOperation(base, spec)
		out = append(out, tool)
	}
	return out
}

func proposalToolForOperation(base openai.Tool, spec actionresolver.OperationSpec) openai.Tool {
	tool := base
	function := *base.Function
	root, ok := cloneSchemaObject(function.Parameters)
	if !ok {
		return base
	}
	properties, _ := root["properties"].(map[string]any)
	delete(properties, "operation")
	root["required"] = []string{"slots"}
	fields := actionresolver.SortedFieldNames(spec)
	if slots, ok := properties["slots"].(map[string]any); ok {
		if items, ok := slots["items"].(map[string]any); ok {
			if slotProperties, ok := items["properties"].(map[string]any); ok {
				slotProperties["name"] = map[string]any{"type": "string", "enum": fields}
			}
		}
	}
	function.Name = proposalToolName(spec.Operation)
	function.Description = strings.TrimSpace(spec.Description) + " 只提出候选，不直接执行。"
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
