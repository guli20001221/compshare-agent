package engine

import (
	"context"
	"strings"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/observability"
)

// Large enough to preserve every evidence item shown during one bounded turn.
const searchKnowledgeLedgerTurnMaxItems = 256

// knowledgeAnswerQuestion keeps the first Agent query as the answer context
// across later searches and citation processing.
func (e *Engine) knowledgeAnswerQuestion(fallback string) string {
	question := strings.TrimSpace(e.searchKnowledgeLedgerThisTurn.Query)
	if question == "" {
		question = strings.TrimSpace(fallback)
		e.searchKnowledgeLedgerThisTurn.Query = question
	}
	return question
}

// finalizeAgentLoopKnowledgeAnswer is the sole SearchKnowledge answer exit. No
// second model reviews or rewrites the answer: the central Agent is the semantic
// decider and the runtime validates citation markers only.
//
// Policy — fail-open, NO hard stop:
//   - An answer carrying >=1 citation that resolves to the current+prior
//     verification ledger, and no unknown citation, is accepted (markers
//     stripped for display).
//   - Otherwise the original answer SHIPS with all citation markers (including
//     fabricated ones) stripped. Citation typography never gets to rewrite a
//     semantically correct answer.
//
// A verbatim evidence echo is recorded for analysis and never changes the reply.
func (e *Engine) finalizeAgentLoopKnowledgeAnswer(_ context.Context, fallbackQuestion, candidate string) string {
	if !e.knowledgeQAAgentLoopThisTurn {
		return candidate
	}
	resolved := e.knowledgeAnswerQuestion(fallbackQuestion)
	ledger := e.knowledgeLedgerForVerification(resolved)

	echoed := e.recordAnswerEvidenceEcho(candidate)
	report := knowledge.ValidateGroundedCitations(candidate, ledger)
	if report.Grounded() {
		return e.acceptGroundedKnowledgeAnswer(resolved, candidate, report, groundingSupported)
	}

	// Fail-open floor: strip any (incl. fabricated) citation markers, ship clean
	// prose. Never a canned whole-answer replacement. The accepted arm carries the
	// echo signal out on its citation trace; this arm emits none, so an echo here
	// needs its own turn-aggregate emission to stay measurable.
	if echoed {
		e.emitSearchKnowledgeTurnTrace(nil)
	}
	e.groundingOutcomeThisTurn = groundingUnavailable
	return knowledge.StripCiteMarkers(candidate)
}

// acceptGroundedKnowledgeAnswer records the citation trace, retracts any standing
// ungrounded hard-block, strips the markers for display, and durably remembers a
// pure-RAG answer (never one built on turn-local read-tool evidence).
func (e *Engine) acceptGroundedKnowledgeAnswer(resolved, answer string, report knowledge.GroundedAnswerReport, outcome string) string {
	e.emitSearchKnowledgeCitationTrace(report)
	e.retractKnowledgeHardBlock()
	display := knowledge.StripCiteMarkers(answer)
	current := e.currentTurnEvidenceLedger(resolved)
	e.groundingCitationScopeThisTurn = groundingCitationScope(current, report.CitedChunkIDs)
	if len(e.platformReadEvidenceThisTurn) == 0 {
		// What is stored is what THIS turn retrieved, not the merged ledger the
		// verifier judged against, and only the current items the answer actually
		// cited. Storing every current candidate would promote unrelated search
		// results; storing the merge would re-stamp prior chunks indefinitely.
		// A prior-only answer therefore stores nothing new, while a mixed answer
		// remembers only its cited current-turn portion.
		e.rememberVerifiedEvidence(resolved, citedEvidence(current, report.CitedChunkIDs))
	}
	e.groundingOutcomeThisTurn = outcome
	return display
}

func groundingCitationScope(current knowledge.EvidenceLedger, citedChunkIDs []string) string {
	if len(citedChunkIDs) == 0 {
		return ""
	}
	currentIDs := make(map[string]struct{}, len(current.Items))
	for _, item := range current.Items {
		if id := strings.TrimSpace(item.ChunkID); id != "" {
			currentIDs[id] = struct{}{}
		}
	}
	hasCurrent := false
	hasPrior := false
	for _, rawID := range citedChunkIDs {
		id := strings.TrimSpace(rawID)
		if id == "" {
			continue
		}
		if _, ok := currentIDs[id]; ok {
			hasCurrent = true
		} else {
			hasPrior = true
		}
	}
	switch {
	case hasCurrent && hasPrior:
		return groundingCitationScopeMixed
	case hasCurrent:
		return groundingCitationScopeCurrentOnly
	case hasPrior:
		return groundingCitationScopePriorOnly
	default:
		return ""
	}
}

func citedEvidence(ledger knowledge.EvidenceLedger, citedChunkIDs []string) knowledge.EvidenceLedger {
	wanted := make(map[string]struct{}, len(citedChunkIDs))
	for _, rawID := range citedChunkIDs {
		if id := strings.TrimSpace(rawID); id != "" {
			wanted[id] = struct{}{}
		}
	}
	out := knowledge.EvidenceLedger{Query: ledger.Query, Items: []knowledge.EvidenceItem{}}
	for _, item := range ledger.Items {
		id := strings.TrimSpace(item.ChunkID)
		if _, ok := wanted[id]; !ok {
			continue
		}
		out.Items = append(out.Items, item)
	}
	return out
}

// recordAnswerEvidenceEcho stamps the turn with the chunk this answer reproduced
// verbatim, for telemetry only, and reports whether there was one. Callers must
// not branch on it beyond making it observable.
func (e *Engine) recordAnswerEvidenceEcho(answer string) bool {
	chunkID := knowledge.EchoedEvidenceChunkID(answer, e.searchKnowledgeHitsThisTurn)
	if chunkID == "" {
		// Citation markers can be inserted inside a 32+ rune excerpt and break the
		// contiguous needle. The user sees the markers stripped, so check that
		// exact display text too.
		chunkID = knowledge.EchoedEvidenceChunkID(knowledge.StripCiteMarkers(answer), e.searchKnowledgeHitsThisTurn)
	}
	if chunkID == "" {
		return false
	}
	e.answerEchoedChunkIDThisTurn = chunkID
	return true
}

func (e *Engine) emitKnowledgeHardBlock(trace observability.EngineHardBlockTrace) {
	e.hardBlockStandingThisTurn = trace.Hit
	if trace.Hit {
		e.hardBlockTraceThisTurn = trace
	} else {
		e.hardBlockTraceThisTurn = observability.EngineHardBlockTrace{}
	}
	if e.hardBlockObserver != nil {
		e.hardBlockObserver(trace)
	}
}

func (e *Engine) retractKnowledgeHardBlock() {
	if !e.hardBlockStandingThisTurn {
		return
	}
	e.hardBlockStandingThisTurn = false
	e.hardBlockTraceThisTurn = observability.EngineHardBlockTrace{}
	if e.hardBlockObserver != nil {
		e.hardBlockObserver(observability.EngineHardBlockTrace{Hit: false})
	}
}
