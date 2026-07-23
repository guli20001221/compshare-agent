package engine

import (
	"context"
	"strings"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/observability"
)

// Large enough to preserve every evidence item shown during one bounded turn.
const searchKnowledgeLedgerTurnMaxItems = 256

// resolvedKnowledgeQuestion is the single question used after retrieval. The
// bounded query planner separates that answer target from its retrieval queries;
// using the short last utterance again (for example "粘贴呢") would split
// retrieval from synthesis. A missing value is repaired once at the boundary so
// every later stage reads the same question.
func (e *Engine) resolvedKnowledgeQuestion(fallback string) string {
	resolved := strings.TrimSpace(e.resolvedKnowledgeQuestionThisTurn)
	if resolved == "" {
		resolved = strings.TrimSpace(e.searchKnowledgeLedgerThisTurn.Query)
	}
	if resolved == "" {
		resolved = strings.TrimSpace(fallback)
	}
	e.resolvedKnowledgeQuestionThisTurn = resolved
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
// Policy (fail-open):
//   - An answer carrying >=1 citation that resolves to a real per-turn ledger
//     chunk is accepted (markers stripped for display).
//   - Otherwise the original answer SHIPS with all citation markers (including
//     fabricated ones) stripped. Citation typography never gets to rewrite a
//     semantically correct answer.
//
// The only hard stop is a raw-evidence leak (a security concern — the answer
// pastes verbatim evidence, which may include read-tool payloads). Precise
// platform facts do not travel this path; they are server-rendered from read tools.
func (e *Engine) finalizeAgentLoopKnowledgeAnswer(_ context.Context, fallbackQuestion, candidate string) string {
	if !e.knowledgeQAAgentLoopThisTurn {
		return candidate
	}
	resolved := e.resolvedKnowledgeQuestion(fallbackQuestion)
	ledger := e.knowledgeLedgerForVerification(resolved)

	leak := knowledgeAnswerHasRawLeak(candidate, e.searchKnowledgeHitsThisTurn)
	report := knowledge.ValidateGroundedCitations(candidate, ledger)
	if !leak && report.Grounded() {
		return e.acceptGroundedKnowledgeAnswer(resolved, candidate, report, groundingSupported)
	}

	// Raw evidence can contain operational payloads and never ships verbatim.
	if leak {
		e.emitSearchKnowledgeHardBlock("search_knowledge_raw_leak")
		e.groundingOutcomeThisTurn = groundingUnsupported
		return ragUngroundableReply
	}

	// Fail-open floor: strip any (incl. fabricated) citation markers, ship clean
	// prose. Never a canned whole-answer replacement.
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
	if len(e.readResponseEvidenceThisTurn) == 0 && len(report.CitedChunkIDs) > 0 {
		e.rememberVerifiedKnowledge(resolved, display, e.knowledgeLedgerForVerification(resolved))
	}
	e.groundingOutcomeThisTurn = outcome
	return display
}

func knowledgeAnswerHasRawLeak(answer string, hits []knowledge.RetrievalHit) bool {
	if knowledge.ValidateNoRawEvidenceLeak(answer, hits) != nil {
		return true
	}
	// Citation markers can be inserted inside a raw 32+ rune excerpt to break the
	// leak detector's contiguous needle. The user sees the markers stripped, so
	// validate that exact display text as well.
	display := knowledge.StripCiteMarkers(answer)
	return knowledge.ValidateNoRawEvidenceLeak(display, hits) != nil
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
