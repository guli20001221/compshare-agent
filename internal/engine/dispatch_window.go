package engine

import (
	"sort"
	"strings"

	openai "github.com/sashabaranov/go-openai"

	"github.com/compshare-agent/internal/actionresolver"
	"github.com/compshare-agent/internal/capability"
	"github.com/compshare-agent/internal/tools"
)

const (
	proposalChargeTypeUserQuoteField  = "charge_type_user_quote"
	proposalImageSourceUserQuoteField = "image_source_user_quote"
	proposalWithoutGpuUserQuoteField  = "without_gpu_user_quote"
)

// centralAgentToolWindow is the grouped P6 capability surface. It intentionally
// does not expose the underlying API tools used by deterministic handlers. Each
// high-level read is a distinct, catalog-generated tool, while every platform
// fact still crosses the same EvidenceEnvelope adapter.
func centralAgentToolWindow(mutatingEnabled, instanceOpsEnabled bool) []openai.Tool {
	registry := tools.DefaultCapabilityRegistry()
	var out []openai.Tool
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
		// Take main's route-based knowledge test, not this branch's older
		// `Name == "SearchKnowledge"`: main generalized it so ReadChunk (added
		// there) is exposed too, and matching by name would silently drop it.
		if capability.Policy.Route == tools.ActionRouteKnowledge || capability.Policy.Route == tools.ActionRouteDiagnosis {
			// DiagnoseInstanceInternals carries an ActionRouteDiagnosis policy (derived
			// from its "Diagnose" prefix) so it would append unconditionally here.
			// Gate it on the in-instance lane being wired, so with the lane off the
			// window is byte-identical to before this tool existed (INV-10).
			if capability.Name == "DiagnoseInstanceInternals" && !instanceOpsEnabled {
				continue
			}
			// The lane's description depends on whether writes are authorized, and this
			// window is the ONLY copy the model reads — so the substitution happens here
			// rather than by mutating the shared registry entry, which several other
			// readers (tests, the label coverage gate) expect to stay literal.
			if capability.Name == "DiagnoseInstanceInternals" {
				tool := capability.Tool
				fn := *tool.Function
				fn.Description = tools.InstanceOpsDescription()
				tool.Function = &fn
				out = append(out, tool)
				continue
			}
			out = append(out, capability.Tool)
		}
	}
	return out
}

// centralAgentKnowledgeToolWindow is the fail-closed public-Q&A surface used by
// untrusted chat channels such as Feishu groups. It exposes only knowledge
// retrieval/read capabilities. Platform reads, diagnoses and every action
// proposal are deliberately absent.
func centralAgentKnowledgeToolWindow() []openai.Tool {
	registry := tools.DefaultCapabilityRegistry()
	var out []openai.Tool
	for _, capability := range registry.All() {
		if !capability.ExposedToAgent || capability.Tool.Function == nil {
			continue
		}
		if capability.Policy.Route == tools.ActionRouteKnowledge {
			out = append(out, capability.Tool)
		}
	}
	return out
}

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
	hasSensitiveField := false
	hasChargeTypePhraseField := false
	hasImageSourcePhraseField := false
	hasWithoutGpuPhraseField := false
	for name, field := range spec.Fields {
		if field.Codec == actionresolver.CodecSensitiveText {
			delete(properties, name)
			hasSensitiveField = true
		}
		if name == "ChargeType" && field.Codec == actionresolver.CodecEnum &&
			spec.Operation == "CreateInstanceWorkflow" {
			hasChargeTypePhraseField = true
			property, ok := properties[name].(map[string]any)
			if ok {
				description, _ := property["description"].(string)
				property["description"] = strings.TrimSpace(description +
					" 若该标准值是根据用户原话做的语义归一化而非逐字复制，同时在 charge_type_user_quote 中填写用户原话片段；用户明确选择按量/Postpay 时也必须填写，不能因为它是默认值而省略。")
			}
		}
		// The quote field is driven off RequiresUserRequest rather than off the
		// field name, so a workflow that later declares another such field gets the
		// evidence channel with it. Without one the field is unfillable: the gate
		// accepts nothing but a verified current-message span.
		if field.RequiresUserRequest && field.Codec == actionresolver.CodecEnum &&
			name == "WithoutGpuSpec" {
			hasWithoutGpuPhraseField = true
			property, ok := properties[name].(map[string]any)
			if ok {
				description, _ := property["description"].(string)
				property["description"] = strings.TrimSpace(description +
					" 填写它时，必须同时在 without_gpu_user_quote 中逐字填写用户当前消息里提出该要求的原话片段。")
			}
		}
		if name == "ImageSource" && field.Codec == actionresolver.CodecEnum {
			hasImageSourcePhraseField = true
			property, ok := properties[name].(map[string]any)
			if ok {
				description, _ := property["description"].(string)
				property["description"] = strings.TrimSpace(description +
					" 若该标准值来自用户当前消息明确指定的镜像来源，同时在 image_source_user_quote 中逐字填写对应原话；若来源来自历史推荐、工具结果或你的判断，则该字段留空。")
			}
		}
	}
	if hasChargeTypePhraseField {
		properties[proposalChargeTypeUserQuoteField] = map[string]any{
			"type":        "string",
			"description": "计费方式原话证据。用户把该方式作为本次创建的明确肯定选择时，必须填写当前消息中的连续原文片段，包括按量/Postpay；否定、比较、询价、转述他人意见或仅提到某方式都不是选择，此时填写空字符串。",
		}
	}
	if hasImageSourcePhraseField {
		properties[proposalImageSourceUserQuoteField] = map[string]any{
			"type":        "string",
			"description": "镜像来源原话证据。仅当用户在当前消息中明确肯定选择平台、社区、自制或共享镜像时，填写对应的连续原文片段；否定、比较、询问、历史推荐或仅提到某来源都不是当前选择，此时填写空字符串。",
		}
	}
	if hasWithoutGpuPhraseField {
		properties[proposalWithoutGpuUserQuoteField] = map[string]any{
			"type": "string",
			"description": "无卡开机原话证据。仅当用户在当前消息中明确要求无卡开机时，填写对应的连续原文片段；" +
				"否定、询问、历史提及，或该判断出自你自己，都不是用户的要求，此时填写空字符串。",
		}
	}
	// A proposal may be intentionally incomplete: Resolver returns the exact
	// missing fields and the Agent then asks only for those. Requiring workflow
	// fields in the model schema makes the model ask in prose before it can call.
	//
	// without_gpu_user_quote is deliberately NOT added to required, unlike the two
	// create quotes: a start normally carries no WithoutGpuSpec at all, so making
	// every start declare an empty quote would spend a field on every call to
	// describe the absence of a rare one. The gate does not depend on it being
	// required — a missing quote is a missing user request, which is a rejection.
	required := []string{}
	if hasChargeTypePhraseField {
		required = append(required, proposalChargeTypeUserQuoteField)
	}
	if hasImageSourcePhraseField {
		required = append(required, proposalImageSourceUserQuoteField)
	}
	root["required"] = required
	function.Name = proposalToolName(spec.Operation)
	// The system prompt owns the action-first / partial-proposal / confirmation
	// rules once. A tool description owns only this operation's semantic boundary;
	// workflow step sequences are runtime details and must never leak in here.
	function.Description = strings.TrimSpace(spec.AgentDescription)
	if hasSensitiveField {
		function.Description += " 敏感值已由服务端安全接收并从参数结构中移除；不要追问或复述该值，直接提交其余已明确字段。"
	}
	function.Parameters = root
	tool.Function = &function
	return tool
}

func proposalArgsForOperation(operation string, direct map[string]any) map[string]any {
	if slots, ok := direct["slots"]; ok {
		return map[string]any{"operation": operation, "slots": slots}
	}
	chargeTypeQuote, _ := direct[proposalChargeTypeUserQuoteField].(string)
	imageSourceQuote, _ := direct[proposalImageSourceUserQuoteField].(string)
	withoutGpuQuote, _ := direct[proposalWithoutGpuUserQuoteField].(string)
	names := make([]string, 0, len(direct))
	for name := range direct {
		if name == proposalChargeTypeUserQuoteField || name == proposalImageSourceUserQuoteField ||
			name == proposalWithoutGpuUserQuoteField {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	slots := make([]any, 0, len(names))
	for _, name := range names {
		slot := map[string]any{"name": name, "value": direct[name]}
		if name == "ChargeType" {
			if quote := strings.TrimSpace(chargeTypeQuote); quote != "" {
				slot["evidence"] = map[string]any{"quote": quote}
			}
		}
		if name == "ImageSource" {
			if quote := strings.TrimSpace(imageSourceQuote); quote != "" {
				slot["evidence"] = map[string]any{"quote": quote}
			}
		}
		if name == "WithoutGpuSpec" {
			if quote := strings.TrimSpace(withoutGpuQuote); quote != "" {
				slot["evidence"] = map[string]any{"quote": quote}
			}
		}
		slots = append(slots, slot)
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

func centralAgentToolNames(mutatingEnabled, instanceOpsEnabled bool) []string {
	window := centralAgentToolWindow(mutatingEnabled, instanceOpsEnabled)
	names := make([]string, 0, len(window))
	for _, tool := range window {
		if tool.Function != nil && tool.Function.Name != "" {
			names = append(names, tool.Function.Name)
		}
	}
	return names
}

// ModelVisibleToolNames returns every tool name the model can be offered, over
// all runtime flag combinations, de-duplicated and sorted.
//
// It exists because the window is assembled from three unrelated sources —
// tools.Registry (+ ShadowCapabilityDefinitions), capability.ReadDefinitions()
// (the "ReadCapability_" + intent family), and the Request<Operation> proposal
// tools generated from the write catalog — and those names are what the console
// prints in its activity stream. A hand-kept display map missed one source at a
// time, three separate times, each caught only by a live run. Presentation code
// covers THIS list instead of re-deriving it, so adding a tool anywhere fails
// the label test rather than shipping a raw English name to users.
func ModelVisibleToolNames() []string {
	seen := map[string]bool{}
	var names []string
	// Every gate the window is built from gets both values. The in-instance
	// SSH-ops lane added the second one after this function was written: with
	// only mutatingEnabled iterated, DiagnoseInstanceInternals never enters this
	// list, so the label test stops covering it and the lane ships a raw English
	// name into the activity stream — the exact failure this list exists to make
	// impossible. A new gate on centralAgentToolNames must be added here too.
	for _, mutatingEnabled := range []bool{false, true} {
		for _, instanceOpsEnabled := range []bool{false, true} {
			for _, name := range centralAgentToolNames(mutatingEnabled, instanceOpsEnabled) {
				if seen[name] {
					continue
				}
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names
}
