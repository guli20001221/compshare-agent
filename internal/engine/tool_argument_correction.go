package engine

import (
	"errors"
	"strings"

	"github.com/compshare-agent/internal/capability"
	"github.com/compshare-agent/internal/tools"
)

// modelOwnedInstanceIDCompletion turns one observable transcription mistake
// into the existing correct_tool_call control result. It deliberately accepts
// only a unique trailing-suffix completion of the model value: merely mentioning
// another instance in the current message must never replace a carried target.
// The rejected call is not edited or executed; the next model round must emit
// the complete value itself.
func modelOwnedInstanceIDCompletion(action string, err error) (tools.AgentToolResult, bool) {
	var mismatch *capability.InstanceIDGroundingMismatch
	if !errors.As(err, &mismatch) || mismatch == nil {
		return tools.AgentToolResult{}, false
	}
	provided := strings.TrimSpace(mismatch.Provided)
	foldedProvided := strings.ToLower(provided)
	if foldedProvided == "" {
		return tools.AgentToolResult{}, false
	}
	var complete string
	for _, literal := range mismatch.UserLiteralIDs {
		literal = strings.TrimSpace(literal)
		foldedLiteral := strings.ToLower(literal)
		if len(foldedLiteral) <= len(foldedProvided) || !strings.HasPrefix(foldedLiteral, foldedProvided) {
			continue
		}
		if complete != "" && !strings.EqualFold(complete, literal) {
			return tools.AgentToolResult{}, false
		}
		complete = literal
	}
	if complete == "" {
		return tools.AgentToolResult{}, false
	}

	result := tools.AgentToolInvalidToolCall(
		action,
		tools.AgentToolCodeInvalidArguments,
		"模型提交的实例 ID 被截短。请使用 data.complete_instance_id 中的用户原文完整值修正目标（以及 source_span，如有）后重发同一次工具调用；不要要求用户重复提供，也不要改查其他实例。",
		tools.AgentToolMeta{SourceStatus: "instance_id_literal_truncated"},
	)
	result.Data = map[string]any{
		"provided_instance_id": provided,
		"complete_instance_id": complete,
	}
	return result, true
}
