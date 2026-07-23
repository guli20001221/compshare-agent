package engine

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
)

const searchKnowledgeBudgetSynthNote = `根据给定问题和证据生成面向用户的最终答案。
仅输出 JSON：{"answer":"最终答案正文"}。
每条来自证据的事实用 [1]、[2] 标注，编号对应 evidence.items 的顺序。
只写证据支持的内容，不整段复制证据，不说明检索、改写、校验或内部处理过程。`

type knowledgeSynthesisInput struct {
	Question string                   `json:"question"`
	Evidence knowledge.EvidenceLedger `json:"evidence"`
}

type knowledgeSynthesisOutput struct {
	Answer string `json:"answer"`
}

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
	client := e.llmClient
	if client == nil {
		return "", false
	}
	payload, err := json.Marshal(knowledgeSynthesisInput{Question: resolved, Evidence: ledger})
	if err != nil {
		return "", false
	}
	resp, err := client.Chat(ctx, llm.ChatRequest{
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: searchKnowledgeBudgetSynthNote},
			{Role: openai.ChatMessageRoleUser, Content: string(payload)},
		},
		ResponseFormat: &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONObject,
		},
	})
	if err != nil || resp == nil {
		return "", false
	}
	e.emitTokenUsage(resp.Usage)
	var output knowledgeSynthesisOutput
	if !parseFirstJSONObject(resp.Content, &output) {
		return "", false
	}
	answer := strings.TrimSpace(output.Answer)
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
