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
	Capability string
	Reply      string
	Envelope   envelope.Envelope
}

// finalizeResponse is the single final-text gateway for the central Agent.
// It does not infer intent from words in the answer. Instead it selects the
// applicable contract from evidence that actually crossed a tool boundary.
func (e *Engine) finalizeResponse(ctx context.Context, userMsg, draft string) string {
	content := e.guardMonitorTemporalFinalReply(draft)
	content = security.RedactOperationalTokensInText(content)

	// SearchKnowledge owns its semantic claim verifier and repair path. When a
	// turn searched, platform read output may be supporting evidence for a
	// diagnosis, so it must not replace the verified knowledge answer.
	content = e.finalizeAgentLoopKnowledgeAnswer(ctx, userMsg, content)
	if len(e.searchKnowledgeLedgerThisTurn.Items) == 0 {
		if canonical := canonicalReadResponse(e.readResponseEvidenceThisTurn); canonical != "" {
			content = canonical
		}
	}

	if corrected, ok := e.correctFalseInstanceNotFoundReply(userMsg, content); ok {
		content = corrected
	}
	if strings.TrimSpace(content) == "" {
		content = emptyReplyFallbackMessage
	}
	if substituted, ok := substituteInstanceTable(content, e.instanceTableThisTurn); ok {
		content = substituted
	}
	return content
}

func canonicalReadResponse(evidence []readResponseEvidence) string {
	seen := map[string]struct{}{}
	parts := make([]string, 0, len(evidence))
	for _, item := range evidence {
		reply := strings.TrimSpace(item.Reply)
		if reply == "" {
			continue
		}
		if _, ok := seen[reply]; ok {
			continue
		}
		seen[reply] = struct{}{}
		parts = append(parts, reply)
	}
	return strings.Join(parts, "\n\n")
}
