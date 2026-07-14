package engine

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	openai "github.com/sashabaranov/go-openai"
)

// knowledgeAnswerVerifierPrompt validates the thing citations were only a proxy
// for: whether every substantive claim in the answer is supported by the exact
// EvidenceLedger shown to the agent. The answer/evidence quote pairs are checked
// again by Go, so "supported":true on its own is never sufficient.
const knowledgeAnswerVerifierPrompt = `你是严格的知识答案事实核查员。只判断答案是否完全由给定证据支持，不补充常识，也不重新回答。

输入中的 resolved_question 与 evidence.query 必须完全一致。对答案中的每一条实质主张：
1. answer_quote 必须逐字摘自答案，并覆盖该主张所在的完整短句或分句。
2. evidence_quote 必须逐字摘自对应 chunk_id 的 snippet、summary 或 title，并明确支持该主张。
3. 找不到明确证据、只能推断、或不确定时，supported=false，并把原主张放入 unsupported。
4. 一个无据主张就使整篇 supported=false。引用标记 [1] 或 [[chunk_id]] 不能证明内容真实。
5. 礼貌语可以忽略；操作步骤、数字、条件、因果、产品规则都属于实质主张。

只输出 JSON：
{"supported":true|false,"claims":[{"answer_quote":"答案原句","chunk_id":"证据ID","evidence_quote":"证据原句"}],"unsupported":["无据主张"]}`

const knowledgeAnswerRepairPrompt = `你是知识答案修复器。只依据输入的 EvidenceLedger 回答 resolved_question，不得使用外部常识。

要求：
1. answer 直接回答问题；没有明确依据的内容不要写。
2. claims 必须覆盖 answer 的每一条实质主张。answer_quote 逐字摘自 answer，evidence_quote 逐字摘自对应证据。
3. 证据不足以回答时 supported=false，answer 留空。
4. 不要大段复制证据原文。引用标记可写可不写，服务端不靠标点判断真实性。

只输出 JSON：
{"answer":"给用户的回答","supported":true|false,"claims":[{"answer_quote":"答案原句","chunk_id":"证据ID","evidence_quote":"证据原句"}],"unsupported":["无据主张"]}`

const knowledgeAnswerDirectnessNote = `
证据已经明确支持的结论要直接说清，不要在结尾又改口为"资料未覆盖"或添加无依据的免责话；但仍不得超出证据。`

type knowledgeGroundingClaim struct {
	AnswerQuote   string `json:"answer_quote"`
	ChunkID       string `json:"chunk_id"`
	EvidenceQuote string `json:"evidence_quote"`
}

type knowledgeGroundingVerdict struct {
	Supported   bool                      `json:"supported"`
	Claims      []knowledgeGroundingClaim `json:"claims"`
	Unsupported []string                  `json:"unsupported"`
}

type knowledgeRepairEnvelope struct {
	Answer string `json:"answer"`
	knowledgeGroundingVerdict
}

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

func (e *Engine) verifyKnowledgeAnswer(ctx context.Context, fallbackQuestion, answer string) (string, knowledge.GroundedAnswerReport, bool) {
	answer = strings.TrimSpace(answer)
	if !knowledgeAnswerVerifierOn || e.llmClient == nil || answer == "" || isKnowledgeRefusal(answer) || len(e.searchKnowledgeLedgerThisTurn.Items) == 0 {
		return "", knowledge.GroundedAnswerReport{}, false
	}
	resolved := e.resolvedKnowledgeQuestion(fallbackQuestion)
	payload, err := json.Marshal(struct {
		ResolvedQuestion string                   `json:"resolved_question"`
		Evidence         knowledge.EvidenceLedger `json:"evidence"`
		Answer           string                   `json:"answer"`
	}{resolved, e.searchKnowledgeLedgerThisTurn, answer})
	if err != nil {
		return "", knowledge.GroundedAnswerReport{}, false
	}
	resp, err := e.llmClient.Chat(ctx, llm.ChatRequest{Messages: []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: knowledgeAnswerVerifierPrompt},
		{Role: openai.ChatMessageRoleUser, Content: string(payload)},
	}})
	if err != nil || resp == nil {
		return "", knowledge.GroundedAnswerReport{}, false
	}
	// There is intentionally no post-call budget check. The call has already
	// been paid for; a valid verdict must not be discarded after the fact.
	e.emitTokenUsage(resp.Usage)
	var verdict knowledgeGroundingVerdict
	if !parseKnowledgeGroundingJSON(resp.Content, &verdict) {
		return "", knowledge.GroundedAnswerReport{}, false
	}
	return validateKnowledgeGroundingProof(answer, verdict, e.searchKnowledgeLedgerThisTurn)
}

// repairKnowledgeAnswerWithProof performs one bounded repair call. The answer
// and its evidence proof are returned together, which avoids paying for a repair
// and then discarding it because a second validation call would cross the budget.
func (e *Engine) repairKnowledgeAnswerWithProof(ctx context.Context, fallbackQuestion string, allowOverBudget bool) (string, knowledge.GroundedAnswerReport, bool) {
	if !knowledgeAnswerVerifierOn || e.llmClient == nil || len(e.searchKnowledgeLedgerThisTurn.Items) == 0 {
		return "", knowledge.GroundedAnswerReport{}, false
	}
	if !allowOverBudget && e.tokenBudgetExceeded() {
		return "", knowledge.GroundedAnswerReport{}, false
	}
	resolved := e.resolvedKnowledgeQuestion(fallbackQuestion)
	payload, err := json.Marshal(struct {
		ResolvedQuestion string                   `json:"resolved_question"`
		Evidence         knowledge.EvidenceLedger `json:"evidence"`
	}{resolved, e.searchKnowledgeLedgerThisTurn})
	if err != nil {
		return "", knowledge.GroundedAnswerReport{}, false
	}
	repairPrompt := knowledgeAnswerRepairPrompt
	if kqaSelfRevisionOn {
		// Preserve the deployed anti-over-conservatism policy inside the single
		// proof-carrying repair call. A separate prose-only revision would need yet
		// another semantic validation call and could reintroduce unsupported claims.
		repairPrompt += knowledgeAnswerDirectnessNote
	}
	resp, err := e.llmClient.Chat(ctx, llm.ChatRequest{Messages: []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: repairPrompt},
		{Role: openai.ChatMessageRoleUser, Content: string(payload)},
	}})
	if err != nil || resp == nil {
		return "", knowledge.GroundedAnswerReport{}, false
	}
	e.emitTokenUsage(resp.Usage)
	var repaired knowledgeRepairEnvelope
	if !parseKnowledgeGroundingJSON(resp.Content, &repaired) {
		return "", knowledge.GroundedAnswerReport{}, false
	}
	answer, report, ok := validateKnowledgeGroundingProof(repaired.Answer, repaired.knowledgeGroundingVerdict, e.searchKnowledgeLedgerThisTurn)
	if !ok || knowledgeAnswerHasRawLeak(repaired.Answer, e.searchKnowledgeHitsThisTurn) {
		return "", knowledge.GroundedAnswerReport{}, false
	}
	return answer, report, true
}

// finalizeAgentLoopKnowledgeAnswer is the only production agent-loop exit for a
// SearchKnowledge answer. Every candidate, including one carrying a syntactically
// valid citation, passes the same model-assisted semantic check and local evidence
// constraints. This reduces unsupported releases but is not a mathematical proof.
// The terminal RAG fallback does not call this function and is intentionally
// unchanged.
func (e *Engine) finalizeAgentLoopKnowledgeAnswer(ctx context.Context, fallbackQuestion, candidate string) string {
	if !e.knowledgeQAAgentLoopThisTurn || !e.searchKnowledgeRanThisTurn {
		return e.guardSearchKnowledgeSynthesis(candidate)
	}
	if len(e.searchKnowledgeHitsThisTurn) == 0 || len(e.searchKnowledgeLedgerThisTurn.Items) == 0 {
		return ragNoEvidenceReply
	}
	_ = e.resolvedKnowledgeQuestion(fallbackQuestion)
	if !knowledgeAnswerVerifierOn {
		e.emitSearchKnowledgeHardBlock("search_knowledge_verifier_disabled")
		return ragUngroundableReply
	}

	leaked := knowledgeAnswerHasRawLeak(candidate, e.searchKnowledgeHitsThisTurn)
	if leaked {
		e.emitSearchKnowledgeHardBlock("search_knowledge_raw_leak")
	} else if domainMatchGuardOn {
		// Product-area inference was intentionally retired with the read-only
		// sub-classifiers. Until a trusted replacement exists, the existing domain
		// guard receives an empty question area and remains fail-open. Do not infer
		// an area from user text here: that would silently revive the removed router.
		if allOff, _ := allCitedOffDomain("", ledgerProductAreas(e.searchKnowledgeLedgerThisTurn)); allOff {
			e.emitSearchKnowledgeHardBlock("search_knowledge_wrong_domain")
			return ragUngroundableReply
		}
	}
	if !leaked {
		if answer, report, ok := e.verifyKnowledgeAnswer(ctx, fallbackQuestion, candidate); ok {
			e.emitSearchKnowledgeCitationTrace(report)
			e.retractKnowledgeHardBlock()
			return answer
		}
		e.emitSearchKnowledgeHardBlock("search_knowledge_ungrounded")
	}

	if disciplinedKnowledgeQASynthesisOn {
		if repaired, ok := e.synthesizeKnowledgeQAFromLedger(ctx, fallbackQuestion); ok {
			return repaired
		}
	}
	// Evidence existed, so claiming that the KB did not cover the topic is
	// false. This remains a refusal, but reports the actual failure.
	return ragUngroundableReply
}

// synthesizeKnowledgeAnswerAfterBudget is the existing one-call rescue policy
// for a turn that already crossed its normal budget. It never generates from an
// empty ledger and validates the answer in the same response.
func (e *Engine) synthesizeKnowledgeAnswerAfterBudget(ctx context.Context, fallbackQuestion string) (string, bool) {
	if len(e.searchKnowledgeHitsThisTurn) == 0 {
		return "", false
	}
	resolved := e.resolvedKnowledgeQuestion(fallbackQuestion)
	if len(e.searchKnowledgeLedgerThisTurn.Items) == 0 {
		e.searchKnowledgeLedgerThisTurn = knowledge.BuildSubstantiveEvidenceLedger(resolved, e.searchKnowledgeHitsThisTurn, searchKnowledgeLedgerTurnMaxItems, 0)
	}
	if len(e.searchKnowledgeLedgerThisTurn.Items) == 0 {
		return "", false
	}
	answer, report, ok := e.repairKnowledgeAnswerWithProof(ctx, fallbackQuestion, true)
	if !ok {
		return "", false
	}
	e.emitSearchKnowledgeCitationTrace(report)
	e.retractKnowledgeHardBlock()
	return answer, true
}

func parseKnowledgeGroundingJSON(raw string, dst any) bool {
	text := strings.TrimSpace(raw)
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return false
	}
	return json.Unmarshal([]byte(text[start:end+1]), dst) == nil
}

func validateKnowledgeGroundingProof(answer string, verdict knowledgeGroundingVerdict, ledger knowledge.EvidenceLedger) (string, knowledge.GroundedAnswerReport, bool) {
	answer = strings.TrimSpace(answer)
	if answer == "" || isKnowledgeRefusal(answer) || !verdict.Supported || len(verdict.Unsupported) > 0 || len(verdict.Claims) == 0 {
		return "", knowledge.GroundedAnswerReport{}, false
	}
	citationReport := knowledge.ValidateGroundedCitations(answer, ledger)
	if len(citationReport.UnknownCitations) > 0 {
		return "", knowledge.GroundedAnswerReport{}, false
	}
	display := strings.TrimSpace(knowledge.StripCiteMarkers(answer))
	items := make(map[string]knowledge.EvidenceItem, len(ledger.Items))
	for _, item := range ledger.Items {
		if id := strings.TrimSpace(item.ChunkID); id != "" {
			items[id] = item
		}
	}
	used := make([]string, 0, len(verdict.Claims))
	seen := map[string]struct{}{}
	validQuotes := make([]string, 0, len(verdict.Claims))
	for _, claim := range verdict.Claims {
		answerQuote := normalizeGroundingText(knowledge.StripCiteMarkers(claim.AnswerQuote))
		evidenceQuote := normalizeGroundingText(claim.EvidenceQuote)
		item, exists := items[strings.TrimSpace(claim.ChunkID)]
		if !exists || answerQuote == "" || evidenceQuote == "" || !strings.Contains(normalizeGroundingText(display), answerQuote) {
			return "", knowledge.GroundedAnswerReport{}, false
		}
		evidenceText := normalizeGroundingText(strings.Join([]string{item.Title, item.Summary, item.Snippet}, " "))
		if !strings.Contains(evidenceText, evidenceQuote) {
			return "", knowledge.GroundedAnswerReport{}, false
		}
		if obviousKnowledgeGroundingContradiction(claim.AnswerQuote, claim.EvidenceQuote) {
			return "", knowledge.GroundedAnswerReport{}, false
		}
		if !groundingQuantitiesConsistent(claim.AnswerQuote, claim.EvidenceQuote) {
			return "", knowledge.GroundedAnswerReport{}, false
		}
		validQuotes = append(validQuotes, answerQuote)
		id := strings.TrimSpace(claim.ChunkID)
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			used = append(used, id)
		}
	}
	for _, segment := range substantiveKnowledgeSegments(display) {
		covered := false
		for _, quote := range validQuotes {
			// The proof must quote the complete substantive clause. Accepting the
			// reverse relation would let a verifier "cover" a long unsupported
			// sentence with a generic three-character fragment such as "可退款".
			if strings.Contains(quote, segment) {
				covered = true
				break
			}
		}
		if !covered {
			return "", knowledge.GroundedAnswerReport{}, false
		}
	}
	if len(used) == 0 {
		return "", knowledge.GroundedAnswerReport{}, false
	}
	return display, knowledge.GroundedAnswerReport{HasCitation: true, CitedChunkIDs: used}, true
}

var knowledgeClauseBoundaryRE = regexp.MustCompile(`[。！？!?；;，,：:\n]+`)

func substantiveKnowledgeSegments(answer string) []string {
	parts := knowledgeClauseBoundaryRE.Split(answer, -1)
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.Trim(normalizeGroundingText(part), " -*#>`~")
		if utf8.RuneCountInString(part) < 3 || isNonSubstantiveKnowledgePhrase(part) {
			continue
		}
		out = append(out, part)
	}
	return out
}

func isNonSubstantiveKnowledgePhrase(s string) bool {
	for _, phrase := range []string{"希望这些信息对你有帮助", "希望对你有帮助", "如需更多帮助请告诉我", "如有其他问题请告诉我"} {
		if strings.Contains(s, phrase) {
			return true
		}
	}
	return false
}

func normalizeGroundingText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(s)), " "))
}

func knowledgeAnswerHasRawLeak(answer string, hits []knowledge.RetrievalHit) bool {
	if knowledge.ValidateNoRawEvidenceLeak(answer, hits) != nil {
		return true
	}
	// Citation markers can be inserted inside a raw 32+ rune excerpt to break
	// the leak detector's contiguous needle. The user sees the markers stripped,
	// so validate that exact display text as well.
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
