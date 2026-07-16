package engine

import (
	"context"
	"strings"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	openai "github.com/sashabaranov/go-openai"
)

// Large enough to preserve every evidence item shown during one bounded turn.
const searchKnowledgeLedgerTurnMaxItems = 256

// searchKnowledgeCiteRetryNote is the single bounded cite-or-drop nudge appended
// to the Agent's OWN context for one same-model retry. There is no second verifier
// persona: the central Agent rewrites its own answer with citations, or drops the
// claim it cannot cite. The wording separates material facts (must cite) from
// general knowledge (need not cite) so the retry never degrades a correct
// stable-general answer into a refusal.
const searchKnowledgeCiteRetryNote = `你上一条回答里有来自本轮资料的事实没有标注引用编号。请重写这条回答：每条来自本轮资料的事实都用 [1]、[2] 这样的编号标注（编号对应本轮证据条目的顺序）；属于通用常识、不来自本轮资料的内容可以不标；既无法从本轮资料引用、又不是通用常识的那一句，请删掉或改成不声称它是事实。不要整段复制资料原文，用自己的话概括。`

// resolvedKnowledgeQuestion is the single question used after retrieval. The
// SearchKnowledge query is already the agent's history-aware rewrite; using the
// short last utterance again (for example "粘贴呢") would split retrieval from
// synthesis. A missing Query is repaired once at the boundary so every later
// stage reads the same value.
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
//   - Otherwise, when there is evidence to cite against, the Agent gets exactly
//     ONE bounded same-model retry to cite or drop the uncited claim.
//   - If it still cannot cite, the answer SHIPS ANYWAY with all citation markers
//     (including fabricated ones) stripped: a wrong or missing chunk_id must not
//     destroy a likely-correct answer. This resolves the chunk_id-mismatch concern
//     by not blocking on it.
//
// The only hard stop is a PERSISTENT raw-evidence leak (a security concern — the
// answer pastes verbatim evidence, which may include read-tool payloads). Precise
// platform facts do not travel this path; they are server-rendered from read tools.
func (e *Engine) finalizeAgentLoopKnowledgeAnswer(ctx context.Context, fallbackQuestion, candidate string) string {
	if !e.knowledgeQAAgentLoopThisTurn {
		return candidate
	}
	resolved := e.resolvedKnowledgeQuestion(fallbackQuestion)
	ledger := e.knowledgeLedgerForVerification(resolved)

	leak := knowledgeAnswerHasRawLeak(candidate, e.searchKnowledgeHitsThisTurn)
	report := knowledge.ValidateGroundedCitations(candidate, ledger)
	if !leak && report.HasCitation {
		return e.acceptGroundedKnowledgeAnswer(resolved, candidate, report, groundingSupported)
	}

	// One bounded same-model retry, only when there is evidence to cite against.
	// A no-evidence direct answer (stable-general) has nothing to cite, so it is
	// never re-rolled — the Agent already owned that decision.
	if len(ledger.Items) > 0 {
		if retried, ok := e.retryKnowledgeCitation(ctx); ok {
			rleak := knowledgeAnswerHasRawLeak(retried, e.searchKnowledgeHitsThisTurn)
			rreport := knowledge.ValidateGroundedCitations(retried, ledger)
			if !rleak && rreport.HasCitation {
				return e.acceptGroundedKnowledgeAnswer(resolved, retried, rreport, groundingRepaired)
			}
			candidate, leak = retried, rleak
		}
	}

	// A persistent raw leak still cannot ship verbatim evidence (security stop).
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

// retryKnowledgeCitation runs exactly one bounded same-model correction. It reuses
// the Agent's own compiled context (buildMessagesForLLM — which already carries the
// per-turn evidence and the draft) plus a single cite-or-drop nudge, so the central
// Agent rewrites its own answer. No Tools are passed (no tool loop) and there is no
// verifier persona. Returns ("", false) on any client/transport failure.
func (e *Engine) retryKnowledgeCitation(ctx context.Context) (string, bool) {
	client := e.agentLLMClient
	if client == nil {
		client = e.llmClient
	}
	if client == nil {
		return "", false
	}
	messages := append(e.buildMessagesForLLM(),
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: searchKnowledgeCiteRetryNote},
	)
	resp, err := client.Chat(ctx, llm.ChatRequest{Messages: messages})
	if err != nil || resp == nil {
		return "", false
	}
	e.emitTokenUsage(resp.Usage)
	retried := strings.TrimSpace(resp.Content)
	if retried == "" {
		return "", false
	}
	return retried, true
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
