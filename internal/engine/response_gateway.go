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
}

// finalizeResponse is the single final-text gateway for the central Agent.
// It does not infer intent from words in the answer. Instead it selects the
// applicable contract from evidence that actually crossed a tool boundary.
func (e *Engine) finalizeResponse(ctx context.Context, userMsg, draft string) string {
	content := e.guardMonitorTemporalFinalReply(draft)
	content = security.RedactOperationalTokensInText(content)

	// SearchKnowledge owns its semantic claim verifier and repair path. Read
	// capabilities only provide observations to the Agent; they never replace
	// the Agent's final response or end the turn on their own.
	content = substituteReadObservationBlocks(content, e.readResponseEvidenceThisTurn)
	if e.searchKnowledgeRanThisTurn {
		content = e.finalizeAgentLoopKnowledgeAnswer(ctx, userMsg, content)
	} else {
		e.groundingOutcomeThisTurn = groundingUnavailable
	}

	if strings.TrimSpace(content) == "" {
		content = emptyReplyFallbackMessage
	}
	return content
}

func substituteReadObservationBlocks(reply string, evidence []readResponseEvidence) string {
	for _, item := range evidence {
		if item.Placeholder == "" || item.Reply == "" {
			continue
		}
		reply = strings.ReplaceAll(reply, item.Placeholder, item.Reply)
	}
	return reply
}
