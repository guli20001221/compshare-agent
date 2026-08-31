package engine

import (
	"context"
	"strings"

	"github.com/compshare-agent/internal/agentprotocol"
	"github.com/compshare-agent/internal/security"
)

const malformedToolProtocolReply = "本次操作没有进入安全确认流程，请重新提交；系统尚未执行任何修改。"

// finalizeResponse is the single final-text gateway for the central Agent.
// It does not infer intent from words in the answer. Instead it selects the
// applicable contract from evidence that actually crossed a tool boundary.
func (e *Engine) finalizeResponse(ctx context.Context, userMsg, draft string) string {
	if security.ContainsToolProtocolMarkup(draft) {
		return malformedToolProtocolReply
	}
	// Customer-support delivery is valid only as the deterministic result of
	// HandoffToCustomerSupport. A model-authored copy of the private marker must
	// not trigger the adapter without the tool call and its trace.
	draft = strings.ReplaceAll(draft, agentprotocol.FeishuCustomerSupportMarker, "")
	// The console marker is a Feishu-adapter completion contract, not ordinary
	// answer text. Keep it only for a turn that explicitly enabled that adapter;
	// a normal Web turn must never persist or display the private marker.
	if !e.feishuConsoleHandoffThisTurn {
		draft = strings.ReplaceAll(draft, agentprotocol.FeishuConsoleHandoffMarker, "")
	}
	content := e.guardMonitorNoDataFinalReply(draft)
	content = security.RedactOperationalTokensInText(content)

	// SearchKnowledge validates only the Agent-authored draft. Ordinary read
	// facts already reached the Agent as tool evidence, and no second read block
	// is composed afterwards by this gateway.
	if e.searchKnowledgeRanThisTurn {
		content = e.finalizeAgentLoopKnowledgeAnswer(ctx, userMsg, content)
	} else {
		e.groundingOutcomeThisTurn = groundingUnavailable
	}
	return e.finishResponseDelivery(userMsg, draft, content)
}

// finalizeHostTerminalResponse applies the same delivery boundary to text the
// server owns (for example a committed-write recovery). It deliberately skips
// knowledge grounding because this text was not authored by the model.
func (e *Engine) finalizeHostTerminalResponse(userMsg, draft string) string {
	content := security.RedactOperationalTokensInText(draft)
	return e.finishResponseDelivery(userMsg, draft, content)
}

func (e *Engine) finishResponseDelivery(userMsg, originalDraft, content string) string {
	content = prependSensitiveReplies(content, e.sensitiveRepliesThisTurn)
	// A user may deliberately paste a short-lived signed download URL and ask
	// for the exact command to run. Preserve only that exact current-turn URL;
	// arbitrary model/tool credentials remain redacted, and HTTP persistence
	// immediately redacts this reply again for history.
	content = security.RestoreUserProvidedCredentialURLs(content, userMsg, originalDraft)

	if strings.TrimSpace(content) == "" {
		// A turn that already handed the user a verbatim block (the billing card,
		// see verbatimReplyPrefix) is NOT an empty turn — the block is the answer and
		// the Agent correctly had nothing to add. Without this, "本次没有生成有效回复"
		// would be appended underneath a complete answer, which is why the Agent
		// padded with generic prose instead of stopping: silence was not a legal
		// outcome. The caller composes the block, so returning "" yields the card alone.
		if len(e.verbatimBlocksThisTurn) > 0 {
			return ""
		}
		content = emptyReplyFallbackMessage
	}
	return content
}

// prependSensitiveReplies is deliberately the only server-side composition for
// read results. All ordinary facts stay in the model's tool observation and are
// never appended to the final answer a second time.
func prependSensitiveReplies(reply string, sensitiveReplies []string) string {
	var values []string
	for _, item := range sensitiveReplies {
		if item = strings.TrimSpace(item); item != "" {
			values = append(values, item)
		}
	}
	if len(values) > 0 {
		prefix := strings.Join(values, "\n\n")
		if strings.TrimSpace(reply) == "" {
			return prefix
		}
		reply = prefix + "\n\n" + strings.TrimSpace(reply)
	}
	return reply
}
