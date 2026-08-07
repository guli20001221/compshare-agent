package engine

import (
	"context"
	"strings"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/observability"
)

// Every SearchKnowledge call contributes at most DefaultEvidenceLedgerMaxItems,
// and the Agent can make at most maxSearchKnowledgeCallsPerTurn calls.
const searchKnowledgeLedgerTurnMaxItems = maxSearchKnowledgeCallsPerTurn * knowledge.DefaultEvidenceLedgerMaxItems

// resolvedKnowledgeQuestion is the user question used after retrieval. The
// Agent itself resolves follow-up references from canonical transcript before it
// chooses a search query; the runtime does not rewrite that query or run a second
// planning model. A missing turn message falls back to the first search query.
func (e *Engine) resolvedKnowledgeQuestion(fallback string) string {
	resolved := strings.TrimSpace(e.lastUserMsg)
	if resolved == "" {
		resolved = strings.TrimSpace(e.searchKnowledgeLedgerThisTurn.Query)
	}
	if resolved == "" {
		resolved = strings.TrimSpace(fallback)
	}
	e.searchKnowledgeLedgerThisTurn.Query = resolved
	return resolved
}

// finalizeAgentLoopKnowledgeAnswer is the sole production agent-loop exit for a
// SearchKnowledge answer. It is DETERMINISTIC-ONLY: no second model reviews or
// rewrites the answer. The central Agent is the semantic decider; the runtime only
// checks citation-marker validity (typography) — it never re-adjudicates whether
// prose is "grounded enough", because that judgment historically deleted correct
// answers (0/50 blocks in production were真无证据; every one shredded a correct,
// evidence-backed answer that merely mis-cited a chunk id).
//
// Policy — fail-open, NO hard stop:
//   - An answer carrying >=1 citation that resolves to a real per-turn ledger
//     chunk is accepted (markers stripped for display).
//   - Otherwise the original answer SHIPS with all citation markers (including
//     fabricated ones) stripped. Citation typography never gets to rewrite a
//     semantically correct answer.
//
// A verbatim evidence echo is RECORDED (retrieval.answer_echoed_chunk_id) and
// never acted on. It used to replace the whole answer with a canned line on the
// stated rationale that raw evidence "may include read-tool payloads" — but only
// KB chunks are ever passed here, and all 1744 of them are acl=customer_safe, so
// there was nothing to protect. What it did cost is real: it is the same
// whole-answer-replacement failure mode as the retired grounding refusal, and it
// fires hardest on runbook answers (36% of the corpus), where the correct fix IS
// a command line.
func (e *Engine) finalizeAgentLoopKnowledgeAnswer(_ context.Context, fallbackQuestion, candidate string) string {
	if !e.knowledgeQAAgentLoopThisTurn {
		return candidate
	}
	resolved := e.resolvedKnowledgeQuestion(fallbackQuestion)
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
	if len(e.platformReadEvidenceThisTurn) == 0 && len(report.CitedChunkIDs) > 0 {
		// What is stored is what THIS turn retrieved, not the merged ledger the
		// verifier judged against. Storing the merge would copy prior chunks into
		// the new entry with a fresh VerifiedAtUnix, so a chunk fetched once would
		// be re-stamped by every later grounded answer and never leave the
		// verifiedKnowledgeMaxTurns window. An answer grounded purely on prior
		// evidence therefore stores nothing new — which is the intended outcome:
		// that evidence already has an entry, and it should age out on its own.
		e.rememberVerifiedKnowledge(resolved, e.currentTurnEvidenceLedger(resolved))
	}
	e.groundingOutcomeThisTurn = outcome
	return display
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
