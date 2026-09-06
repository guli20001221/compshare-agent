package engine

import (
	"encoding/json"
	"strings"

	"github.com/compshare-agent/internal/actionresolver"
	"github.com/compshare-agent/internal/tools"
)

// agentToolObservation is the one boundary at which a handler result becomes a
// model-visible tool message. Handlers may keep their typed domain payloads
// (read observations, resolver output, workflow data); this adapter supplies a
// single control plane around them.
//
// Deterministic final replies are deliberately handled before this boundary:
// they end the turn and are not supplied to another model round. Every ordinary
// result that can influence a subsequent Agent decision passes through here.
func agentToolObservation(action, raw string) string {
	if _, ok := tools.ParseAgentToolResult(raw); ok {
		return raw
	}

	var data any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return tools.MarshalAgentToolResult(tools.AgentToolFailure(
			action,
			nil,
			"UNSTRUCTURED_TOOL_RESULT",
			"工具返回了无法解析的结果，不能据此继续执行操作。",
			tools.AgentToolMeta{SourceStatus: "unstructured"},
		))
	}
	object, ok := data.(map[string]any)
	if !ok {
		return tools.MarshalAgentToolResult(tools.AgentToolSuccess(action, data, tools.AgentToolMeta{}))
	}

	status := stringField(object, "status")
	meta := tools.AgentToolMeta{SourceStatus: status}
	// Repair a malformed model proposal before asking the user for any other
	// missing field. Otherwise the same invalid field survives the next call and
	// the user is asked to compensate for an error the Agent can fix itself.
	if modelOwnsRejectedProposal(object["rejection_details"]) {
		result := tools.AgentToolInvalidToolCall(
			action,
			tools.AgentToolCodeInvalidArguments,
			"工具参数不符合该操作的声明。请根据 data.rejection_details 修正字段后重新调用；不要要求用户重复已经提供的信息。",
			meta,
		)
		result.Data = toolObservationData(object)
		return tools.MarshalAgentToolResult(result)
	}
	if missing := stringFields(object, "missing_fields", "missing"); len(missing) > 0 {
		meta.MissingFields = missing
		// A missing operation target is an incomplete tool call, not proof that
		// the user has not identified it in the conversation or prior reads.
		if catalog, err := defaultActionCatalog(); err == nil {
			if spec, ok := catalog.Lookup(stringField(object, "operation")); ok {
				for _, name := range missing {
					if !spec.Fields[name].Target {
						continue
					}
					result := tools.AgentToolInvalidToolCall(action, tools.AgentToolCodeInvalidArguments,
						"本次工具调用缺少操作目标 ID。请先根据用户消息、对话历史或工具结果补全目标后重新调用；确实无法确定目标时才询问用户，不要要求用户重复已经提供的信息。", meta)
					result.Data = toolObservationData(object)
					return tools.MarshalAgentToolResult(result)
				}
			}
		}
		return tools.MarshalAgentToolResult(tools.AgentToolNeedsInput(
			action,
			toolObservationData(object),
			"MISSING_REQUIRED_FIELDS",
			"还缺少完成本次请求所需的信息。",
			meta,
		))
	}

	// The typed read capability vocabulary maps cleanly to the five Agent
	// dispositions. Keep this mapping here rather than teaching every read
	// vertical about the outer Agent contract.
	switch status {
	case "needs_input", "fallback_before_tool":
		return tools.MarshalAgentToolResult(tools.AgentToolNeedsInput(
			action, toolObservationData(object), "READ_INPUT_INCOMPLETE", "查询条件不完整或无效。", meta))
	case "conflict":
		return tools.MarshalAgentToolResult(tools.AgentToolChooseAlternative(
			action, toolObservationData(object), "AMBIGUOUS_SELECTION", "存在多个可选对象，需要用户明确选择。", meta))
	case "unavailable":
		return tools.MarshalAgentToolResult(tools.AgentToolChooseAlternative(
			action, toolObservationData(object), "CAPABILITY_UNAVAILABLE", "当前能力不可用，请选择支持的替代方式。", meta))
	case "failure_after_tool":
		return tools.MarshalAgentToolResult(tools.AgentToolRetryLater(
			action, toolObservationData(object), "UPSTREAM_READ_FAILED", "上游查询暂时失败，请稍后重试。", meta))
	case "call_budget_exhausted":
		return tools.MarshalAgentToolResult(tools.AgentToolFailure(
			action, toolObservationData(object), "TOOL_CALL_LIMIT_REACHED", "本回合该能力调用次数已达上限，请根据已有结果回答或追问。", meta))
	case "reused_observation":
		return tools.MarshalAgentToolResult(tools.AgentToolSuccess(action, toolObservationData(object), meta))
	}

	// Action resolver keeps the four causes separate. Preserve that distinction
	// for the model instead of reducing every non-ready proposal to an opaque
	// failure or telling it to ask for a value after an upstream dependency died.
	if len(stringFields(object, "conflicts")) > 0 || nonEmptyCollection(object["conflicts"]) {
		return tools.MarshalAgentToolResult(tools.AgentToolChooseAlternative(
			action, toolObservationData(object), "CONFLICTING_SELECTION", "候选值存在冲突，需要用户选择。", meta))
	}
	if nonEmptyCollection(object["rejected"]) {
		data := toolObservationData(object)
		return tools.MarshalAgentToolResult(tools.AgentToolNeedsInput(
			action, data, "INVALID_FIELD_VALUE", "用户提供的字段不符合该操作要求，需要用户修正。", meta))
	}
	if nonEmptyCollection(object["dependency_failures"]) {
		return tools.MarshalAgentToolResult(tools.AgentToolRetryLater(
			action, toolObservationData(object), "DEPENDENCY_UNAVAILABLE", "服务端暂时无法校验所需资源，请稍后重试。", meta))
	}
	// A failed sibling query does not invalidate candidates a successful search
	// returned. Preserve the partial-outage facts while allowing their inspection.
	if searchKnowledgeHasNoCitableEvidence(action, object) && nonEmptyCollection(object["below_floor_candidates"]) {
		return tools.MarshalAgentToolResult(tools.AgentToolCandidatesNeedInspection(
			action, toolObservationData(object), meta))
	}
	if _, hasError := object["error"]; hasError || (!boolField(object, "success") && hasField(object, "success")) {
		return tools.MarshalAgentToolResult(tools.AgentToolFailure(
			action, toolObservationData(object), "TOOL_REQUEST_FAILED", "工具未能完成本次请求。", meta))
	}
	if searchKnowledgeHasNoCitableEvidence(action, object) {
		data := toolObservationData(object)
		return tools.MarshalAgentToolResult(tools.AgentToolNoCitableEvidence(
			action,
			data,
			meta,
		))
	}

	return tools.MarshalAgentToolResult(tools.AgentToolSuccess(action, toolObservationData(object), meta))
}

// searchKnowledgeHasNoCitableEvidence is deliberately action-specific. A
// successful empty list from a platform read is a factual answer ("未找到"),
// whereas an empty knowledge ledger cannot establish or negate a platform
// policy, current catalogue value, or other evidence-required claim.
func searchKnowledgeHasNoCitableEvidence(action string, object map[string]any) bool {
	if !strings.EqualFold(strings.TrimSpace(action), "SearchKnowledge") {
		return false
	}
	if boolField(object, "empty") {
		return true
	}
	ledger, ok := object["EvidenceLedger"].(map[string]any)
	if !ok {
		return false
	}
	items, ok := ledger["items"]
	return ok && !nonEmptyCollection(items)
}

// toolObservationData removes legacy control fields from the factual payload.
// The typed outer error carries the safe, actionable message, while meta keeps
// the source status. That prevents two competing root-level status vocabularies
// from reaching the model and keeps raw RetCode text out of data.
func toolObservationData(object map[string]any) map[string]any {
	data := make(map[string]any, len(object))
	for key, value := range object {
		if key == "error" || key == "status" || key == "rejected" {
			continue
		}
		data[key] = value
	}
	return data
}

// modelOwnsRejectedProposal reads only the value-free resolver classification.
// Older/untyped results fall back to ask_user rather than assuming ownership.
// If a proposal contains both model- and user-owned problems, the model repairs
// its own call first; a remaining user problem is surfaced on the next attempt.
func modelOwnsRejectedProposal(raw any) bool {
	details, ok := raw.([]any)
	if !ok || len(details) == 0 {
		return false
	}
	for _, item := range details {
		detail, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if stringField(detail, "actor") == string(actionresolver.RejectionActorModel) {
			return true
		}
	}
	return false
}

func stringField(object map[string]any, key string) string {
	value, _ := object[key].(string)
	return strings.TrimSpace(value)
}

func boolField(object map[string]any, key string) bool {
	value, _ := object[key].(bool)
	return value
}

func hasField(object map[string]any, key string) bool {
	_, ok := object[key]
	return ok
}

func stringFields(object map[string]any, keys ...string) []string {
	for _, key := range keys {
		value, ok := object[key]
		if !ok {
			continue
		}
		var out []string
		switch typed := value.(type) {
		case []string:
			for _, item := range typed {
				if item = strings.TrimSpace(item); item != "" {
					out = append(out, item)
				}
			}
		case []any:
			for _, item := range typed {
				if text, ok := item.(string); ok {
					if text = strings.TrimSpace(text); text != "" {
						out = append(out, text)
					}
					continue
				}
				if field, ok := item.(map[string]any); ok {
					if name := strings.TrimSpace(stringField(field, "name")); name != "" {
						out = append(out, name)
					}
				}
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

func nonEmptyCollection(value any) bool {
	switch typed := value.(type) {
	case []any:
		return len(typed) > 0
	case []string:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	default:
		return false
	}
}
