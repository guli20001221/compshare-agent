package engine

import (
	"context"
	"strings"

	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/security"
)

// readResponseEvidence is server-produced factual output, not model prose.
// It is turn-local and is never persisted as a second context representation.
type readResponseEvidence struct {
	Capability  string
	Reply       string
	Envelope    envelope.Envelope
	Placeholder string
	Required    bool
}

const malformedToolProtocolReply = "本次操作没有进入安全确认流程，请重新提交；系统尚未执行任何修改。"

// finalizeResponse is the single final-text gateway for the central Agent.
// It does not infer intent from words in the answer. Instead it selects the
// applicable contract from evidence that actually crossed a tool boundary.
func (e *Engine) finalizeResponse(ctx context.Context, userMsg, draft string) string {
	if security.ContainsToolProtocolMarkup(draft) {
		return malformedToolProtocolReply
	}
	content := e.guardMonitorNoDataFinalReply(draft)
	content = security.RedactOperationalTokensInText(content)

	// SearchKnowledge validates only Agent-authored documentary claims.
	// Deterministic read blocks are composed afterwards, so an unrelated RAG
	// attempt cannot strip or rewrite exact instance/price/monitor facts.
	if e.searchKnowledgeRanThisTurn {
		content = e.finalizeAgentLoopKnowledgeAnswer(ctx, userMsg, content)
	} else {
		e.groundingOutcomeThisTurn = groundingUnavailable
	}
	content = substituteReadObservationBlocks(content, e.readResponseEvidenceThisTurn)

	if strings.TrimSpace(content) == "" {
		content = emptyReplyFallbackMessage
	}
	return content
}

func substituteReadObservationBlocks(reply string, evidence []readResponseEvidence) string {
	var missingRequired []string
	for _, item := range evidence {
		if item.Placeholder == "" || item.Reply == "" {
			continue
		}
		replaced := strings.ReplaceAll(reply, item.Placeholder, item.Reply)
		present := replaced != reply
		reply = replaced
		if item.Required && !present {
			missingRequired = append(missingRequired, item.Reply)
		}
	}
	if len(missingRequired) > 0 {
		prefix := strings.Join(missingRequired, "\n")
		if strings.TrimSpace(reply) == "" {
			return prefix
		}
		reply = prefix + "\n\n" + strings.TrimSpace(reply)
	}
	return reply
}
