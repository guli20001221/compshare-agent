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

// proposalInvocationContract is the ONE model-visible statement of the write
// proposal contract, shared verbatim by every generated Request<Operation> tool.
// The system prompt (segmentCentralAgentBehavior) carries the high-level behavior
// rule; this is the per-tool, schema-level restatement. These two are the single
// sources — the base ProposeAction template deliberately does NOT restate the
// contract a third time.
const (
	proposalInvocationContractPrefix = "提交本操作的结构化候选，参数可不完整；服务端负责缺失字段、来源核验、确认和执行。"
	proposalInvocationContractSuffix = "本工具不直接执行。"
)

func proposalToolName(operation string) string {
	return "Request" + strings.TrimSuffix(operation, "Workflow")
}

func proposalOperationForTool(name string) (string, bool) {
	catalog, err := defaultActionCatalog()
	if err != nil {
		return "", false
	}
	for _, operation := range catalog.Operations() {
		if name == proposalToolName(operation) {
			return operation, true
		}
	}
	return "", false
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
	capability, ok := tools.DefaultCapabilityRegistry().Lookup(spec.Operation)
	if !ok || capability.Tool.Function == nil {
		return base
	}
	root, ok := cloneSchemaObject(capability.Tool.Function.Parameters)
	if !ok {
		return base
	}
	properties, _ := root["properties"].(map[string]any)
	for name, field := range spec.Fields {
		if field.Codec == actionresolver.CodecSensitiveText {
			delete(properties, name)
		}
	}
	// A proposal may be intentionally incomplete: Resolver returns the exact
	// missing fields and the Agent then asks only for those. Requiring workflow
	// fields in the model schema makes the model ask in prose before it can call.
	root["required"] = []string{}
	function.Name = proposalToolName(spec.Operation)
	function.Description = proposalInvocationContractPrefix +
		" " + strings.TrimSpace(spec.Description) + " " + proposalInvocationContractSuffix
	function.Parameters = root
	tool.Function = &function
	return tool
}

// continueWithoutWriteName is the internal first-decision tool the central Agent
// picks when this turn is NOT an immediate write. It is never a catalog operation,
// carries no arguments, and is only ever present in the forced first-decision window.
const continueWithoutWriteName = "ContinueWithoutWrite"

// continueWithoutWriteTool is the single non-Request* option in the forced
// first-decision window. Choosing it commits the turn to read-only/answer/RAG:
// the engine removes every Request* tool for the rest of the turn, so a write
// proposal can only ever happen as the very first decision. Its description is
// the ONLY place the write-vs-not boundary is tuned — deliberately, so the
// choice stays the model's semantic judgment with no keyword table or intent enum.
func continueWithoutWriteTool() openai.Tool {
	return openai.Tool{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name: continueWithoutWriteName,
			Description: "本轮不提交任何写操作。仅当用户是在提问、查询状态/库存/价格、咨询方法或请求推荐（并未要求你现在就动手创建或变更资源）时选择本工具，之后用只读能力和知识检索作答。" +
				"注意：只要用户表达了创建/开机/关机/重装/改配置等对某类资源的写意图，即使只给了部分参数、可用区或镜像等细节还没定，也【不要】选本工具，而应直接提交对应的 Request 操作——服务端会用引导表单补齐缺失字段并让用户确认，你不需要先查询库存/价格/镜像再创建。",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}
}

// firstDecisionToolWindow is the forced-first-decision tool set: every
// catalog-generated Request* proposal tool plus continueWithoutWrite — and
// nothing else (no reads, no SearchKnowledge, no task-state). With
// tool_choice=required the central Agent must commit, on its first hop, to
// either a concrete write proposal or continue-without-write. This is what stops
// the model from free-rolling dozens of read calls before deciding whether it is
// even a write turn.
func firstDecisionToolWindow() []openai.Tool {
	registry := tools.DefaultCapabilityRegistry()
	var out []openai.Tool
	if capability, ok := registry.Lookup(tools.ProposeActionName); ok {
		out = append(out, proposalToolsFromCatalog(capability.Tool)...)
	}
	out = append(out, continueWithoutWriteTool())
	return out
}

// isProposalToolName reports whether name is a catalog-generated Request* tool.
func isProposalToolName(name string) bool {
	_, ok := proposalOperationForTool(name)
	return ok
}

// toolWindowWithoutProposals returns w with every Request* proposal tool and the
// continueWithoutWrite tool removed. Used for rounds after the first decision:
// once the Agent chose continue-without-write (or already made its one write
// proposal), no further write proposal is permitted this turn.
func toolWindowWithoutProposals(w []openai.Tool) []openai.Tool {
	out := make([]openai.Tool, 0, len(w))
	for _, t := range w {
		if t.Function != nil {
			if isProposalToolName(t.Function.Name) || t.Function.Name == continueWithoutWriteName {
				continue
			}
		}
		out = append(out, t)
	}
	return out
}

func proposalArgsForOperation(operation string, direct map[string]any) map[string]any {
	if slots, ok := direct["slots"]; ok {
		return map[string]any{"operation": operation, "slots": slots}
	}
	names := make([]string, 0, len(direct))
	for name := range direct {
		names = append(names, name)
	}
	sort.Strings(names)
	slots := make([]any, 0, len(names))
	for _, name := range names {
		slots = append(slots, map[string]any{"name": name, "value": direct[name]})
	}
	return map[string]any{"operation": operation, "slots": slots}
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
