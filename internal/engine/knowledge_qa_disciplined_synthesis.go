package engine

import (
	"context"
	"strings"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
)

// searchKnowledgeBudgetSynthNote asks the central Agent to write its final answer NOW
// from the evidence it already gathered, for the case where the turn ran out of token
// budget or ReAct rounds before it produced one. Single central-Agent model call, same
// context, no verifier / repair persona. It owns only its STATE — the framing and the
// scope clause (write only evidence-supported content) — while the citation rule itself
// is the shared buildRAGAnswerStrategyNote source.
var searchKnowledgeBudgetSynthNote = buildRAGAnswerStrategyNote(
	`本轮已经检索到资料但还没有给出最终回答。请立即根据本轮已检索到的资料写出最终回答：`,
	`只写有资料支撑的内容，无法支撑的不要写；`,
)

// synthesizeOnBudgetExceeded writes a final answer from the evidence SearchKnowledge
// already gathered this turn, for the budget / round-ceiling recovery paths: when
// the loop cannot finish normally but evidence is in hand, deliver an answer
// grounded on it rather than discarding the turn for a bare "请简化问题".
//
// It is a SINGLE central-Agent model call (no verifier persona, no repair prompt),
// and its output goes through the same deterministic, fail-open citation handling
// as the normal exit: markers that resolve are recorded, all markers are stripped
// for display, and a non-leaking answer ships even if uncited. Returns ("", false)
// only when nothing was retrieved (the "no evidence → refuse, never fabricate"
// guard), the call fails, or the answer would ship a verbatim evidence leak — in
// which case the caller keeps the canned budget refusal.
func (e *Engine) synthesizeOnBudgetExceeded(ctx context.Context, userMsg string) (string, bool) {
	resolved := e.resolvedKnowledgeQuestion(userMsg)
	// The recovery paths can fire before the SearchKnowledge handler folded its
	// hits into the per-turn ledger; build it from the gathered hits so evidence
	// in hand is never discarded for a bare budget refusal.
	if len(e.searchKnowledgeLedgerThisTurn.Items) == 0 && len(e.searchKnowledgeHitsThisTurn) > 0 {
		e.searchKnowledgeLedgerThisTurn = knowledge.BuildSubstantiveEvidenceLedger(resolved, e.searchKnowledgeHitsThisTurn, searchKnowledgeLedgerTurnMaxItems, 0)
	}
	ledger := e.knowledgeLedgerForVerification(resolved)
	if len(ledger.Items) == 0 {
		return "", false
	}
	client := e.agentLLMClient
	if client == nil {
		client = e.llmClient
	}
	if client == nil {
		return "", false
	}
	messages := append(e.buildMessagesForLLM(),
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: searchKnowledgeBudgetSynthNote},
	)
	resp, err := client.Chat(ctx, llm.ChatRequest{Messages: messages})
	if err != nil || resp == nil {
		return "", false
	}
	e.emitTokenUsage(resp.Usage)
	answer := strings.TrimSpace(resp.Content)
	if answer == "" {
		return "", false
	}
	// A recovery answer must not ship a verbatim evidence dump (security); fall
	// back to the budget refusal rather than leak.
	if knowledgeAnswerHasRawLeak(answer, e.searchKnowledgeHitsThisTurn) {
		return "", false
	}
	if report := knowledge.ValidateGroundedCitations(answer, ledger); report.HasCitation {
		e.emitSearchKnowledgeCitationTrace(report)
		if len(e.readResponseEvidenceThisTurn) == 0 && len(report.CitedChunkIDs) > 0 {
			e.rememberVerifiedKnowledge(resolved, knowledge.StripCiteMarkers(answer), ledger)
		}
	}
	e.retractKnowledgeHardBlock()
	e.groundingOutcomeThisTurn = groundingRepaired
	return knowledge.StripCiteMarkers(answer), true
}
