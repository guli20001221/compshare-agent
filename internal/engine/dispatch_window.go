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
)

// centralAgentToolWindow is the grouped P6 capability surface. It intentionally
// does not expose the underlying API tools used by deterministic handlers. Each
// high-level read is a distinct, catalog-generated tool, while every platform
// fact still crosses the same EvidenceEnvelope adapter.
func centralAgentToolWindow(mutatingEnabled, instanceOpsEnabled bool) []openai.Tool {
	return centralAgentToolWindowWithWebSearch(mutatingEnabled, instanceOpsEnabled, false, false)
}

// centralAgentToolWindowWithWebSearch adds at most two dynamic capabilities
// after curated-KB retrieval. A non-empty ledger first exposes the local
// evidence-coverage assessment; only an empty ledger or an assessed coverage
// gap exposes the external-search fallback. Keeping the normal, static function
// above preserves the off-by-default window and makes both dynamic additions
// explicit at the call site.
func centralAgentToolWindowWithWebSearch(mutatingEnabled, instanceOpsEnabled, webSearchAssessmentAvailable, webSearchAvailable bool) []openai.Tool {
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
	if webSearchAssessmentAvailable {
		out = append(out, assessKnowledgeEvidenceTool())
	}
	if webSearchAvailable {
		out = append(out, webSearchTool())
	}
	return out
}

func assessKnowledgeEvidenceTool() openai.Tool {
	return openai.Tool{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "AssessKnowledgeEvidence",
			Description: "仅在本轮 SearchKnowledge 已成功返回可引用知识后才会出现。逐项核对已返回的知识是否直接支持回答用户问题；若节选不足，先用 ReadChunk 阅读已返回 chunk 的全文。只有在全文仍缺少某个必要事实、条件或当前外部技术信息时，才填写 verdict=insufficient，并精确说明缺口及一条不含凭据或个人信息的公开检索语句。不要因为想增加来源、复述同一事实或猜测知识库过期而判定不足。该工具不联网、不会回答用户。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"verdict": map[string]any{
						"type":        "string",
						"enum":        []string{"sufficient", "insufficient"},
						"description": "sufficient 表示现有 KB 证据已覆盖回答所需事实；insufficient 表示明确的必要事实仍缺失。",
					},
					"missing_aspect": map[string]any{
						"type":        "string",
						"description": "仅 verdict=insufficient 时填写：尚不能由 KB 直接证实的具体事实或条件；sufficient 时留空。",
					},
					"external_query": map[string]any{
						"type":        "string",
						"description": "仅 verdict=insufficient 时填写：针对 missing_aspect 的简短公开检索语句；可使用用户问题和 KB 中出现的非敏感产品名或技术术语，不得包含账户、实例、凭据或个人信息；sufficient 时留空。",
					},
				},
				"required": []string{"verdict"},
			},
		},
	}
}

func webSearchTool() openai.Tool {
	return openai.Tool{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "SearchWeb",
			Description: "仅在本轮 SearchKnowledge 没有可引用证据，或 AssessKnowledgeEvidence 已确认现有证据不足后才会出现。无需传参数；系统只会发送此前经覆盖评估保存的、无敏感信息的检索语句。结果是外部补充资料，不是平台现行规则。若采用任何结果，必须在对应结论后附其返回的 Markdown 链接；不能仅凭外部资料断定平台计费、配额、回收、价格、可用性或支持渠道。",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
				"required":   []string{},
			},
		},
	}
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
	// A proposal may be intentionally incomplete: Resolver returns the exact
	// missing fields and the Agent then asks only for those. Requiring workflow
	// fields in the model schema makes the model ask in prose before it can call.
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
	names := make([]string, 0, len(direct))
	for name := range direct {
		if name == proposalChargeTypeUserQuoteField || name == proposalImageSourceUserQuoteField {
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
	return centralAgentToolNamesWithWebSearch(mutatingEnabled, instanceOpsEnabled, false, false)
}

func centralAgentToolNamesWithWebSearch(mutatingEnabled, instanceOpsEnabled, webSearchAssessmentAvailable, webSearchAvailable bool) []string {
	window := centralAgentToolWindowWithWebSearch(mutatingEnabled, instanceOpsEnabled, webSearchAssessmentAvailable, webSearchAvailable)
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
			for _, webSearchAssessmentAvailable := range []bool{false, true} {
				for _, webSearchAvailable := range []bool{false, true} {
					for _, name := range centralAgentToolNamesWithWebSearch(mutatingEnabled, instanceOpsEnabled, webSearchAssessmentAvailable, webSearchAvailable) {
						if seen[name] {
							continue
						}
						seen[name] = true
						names = append(names, name)
					}
				}
			}
		}
	}
	sort.Strings(names)
	return names
}
