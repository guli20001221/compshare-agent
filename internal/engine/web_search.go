package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/compshare-agent/internal/guardrails"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/websearch"
)

const (
	webSearchAction                = "SearchWeb"
	assessKnowledgeEvidenceAction  = "AssessKnowledgeEvidence"
	maxWebSearchMissingAspectRunes = 240
)

// webSearchMayRun is intentionally stricter than "a client exists". It is the
// execution-boundary counterpart to the dynamic tool-window gate: a hallucinated
// tool call cannot turn web search into a first-hop source, and a second query
// cannot spread more of a user turn across an external provider.
func (e *Engine) webSearchMayRun() bool {
	return e != nil && e.webSearcher != nil && !e.webSearchSuppressedThisTurn && e.webSearchAvailableThisTurn &&
		strings.TrimSpace(e.webSearchQueryThisTurn) != "" && e.webSearchCallsThisTurn < maxWebSearchCallsPerTurn
}

// webSearchAssessmentMayRun is deliberately a separate gate from webSearchMayRun.
// A relevant KB hit can still be insufficient for a compound question. There is
// no deterministic score threshold that can decide semantic coverage, so the
// Agent first records a structured, evidence-bound coverage assessment. That
// assessment cannot itself make a network request and has to name one concrete
// gap before it makes SearchWeb visible.
func (e *Engine) webSearchAssessmentMayRun() bool {
	return e != nil && e.webSearcher != nil && !e.webSearchSuppressedThisTurn &&
		e.webSearchAssessmentPendingThisTurn && !e.webSearchAvailableThisTurn &&
		e.webSearchCallsThisTurn < maxWebSearchCallsPerTurn &&
		len(e.searchKnowledgeLedgerThisTurn.Items) > 0
}

// setWebSearchEligibilityAfterKnowledge is the sole producer of the dynamic
// external-search state after a KB call. A successful empty ledger opens the
// ordinary fallback directly. A non-empty ledger never does: it gives the Agent
// a chance to read the cited chunk and then make a local coverage decision.
// An unavailable KB call opens neither path.
func (e *Engine) setWebSearchEligibilityAfterKnowledge(successfulQueries int) {
	if e == nil {
		return
	}
	e.webSearchAvailableThisTurn = false
	e.webSearchAssessmentPendingThisTurn = false
	e.webSearchQueryThisTurn = ""
	if e.webSearcher == nil || e.webSearchSuppressedThisTurn || successfulQueries <= 0 {
		return
	}
	if len(e.searchKnowledgeLedgerThisTurn.Items) > 0 {
		e.webSearchAssessmentPendingThisTurn = true
		return
	}
	// A fully empty, available search has no KB terms to narrow the query. The
	// resolved question is the same question the retrieval planner just searched;
	// still reject it before it can ever leave the process if it contains PII or
	// a credential.
	query := strings.TrimSpace(e.resolvedKnowledgeQuestionThisTurn)
	if query == "" {
		query = strings.TrimSpace(e.lastUserMsg)
	}
	if webSearchQueryIsSafe(query) {
		e.webSearchQueryThisTurn = query
		e.webSearchAvailableThisTurn = true
	}
}

// executeAssessKnowledgeEvidence accepts the one local semantic judgement that
// can turn a non-empty KB result into a search fallback. It does not trust a
// free-form final answer: its compact schema binds the judgement, named gap and
// outbound query together, and the execution boundary validates the latter
// before any external provider is called.
func (e *Engine) executeAssessKnowledgeEvidence(args map[string]any, onStep func(StepEvent)) string {
	if !e.webSearchAssessmentMayRun() {
		message := "知识证据覆盖评估仅能在本轮已成功取得可引用知识、且尚未开放联网检索时进行。"
		onStep(StepEvent{Type: StepBlocked, Action: assessKnowledgeEvidenceAction, Source: observability.ToolSourceMainReAct, Message: message})
		return knowledgeEvidenceAssessmentJSON("not_available", "", "", message)
	}
	verdict, missingAspect, query, err := parseKnowledgeEvidenceAssessment(args)
	if err != nil {
		message := "知识证据覆盖评估参数无效：" + err.Error()
		onStep(StepEvent{Type: StepError, Action: assessKnowledgeEvidenceAction, Source: observability.ToolSourceMainReAct, Message: message})
		return knowledgeEvidenceAssessmentJSON("invalid_request", "", "", message)
	}
	e.webSearchAssessmentPendingThisTurn = false
	if verdict == "sufficient" {
		message := "现有知识证据覆盖完整，无需联网补充。"
		onStep(StepEvent{Type: StepToolResult, Action: assessKnowledgeEvidenceAction, Source: observability.ToolSourceMainReAct, Message: message, TraceResult: map[string]any{"verdict": verdict}})
		return knowledgeEvidenceAssessmentJSON(verdict, "", "", message)
	}
	// The query is stored before SearchWeb is exposed, rather than accepted by
	// SearchWeb itself. That freezes the exact reviewed value across the two
	// model rounds and makes a later invented argument incapable of changing
	// what is disclosed to the provider.
	e.webSearchQueryThisTurn = query
	e.webSearchAvailableThisTurn = true
	message := "知识库证据未覆盖该具体要点；可联网检索补充公开资料。"
	onStep(StepEvent{Type: StepToolResult, Action: assessKnowledgeEvidenceAction, Source: observability.ToolSourceMainReAct, Message: message, TraceResult: map[string]any{"verdict": verdict, "missing_aspect": missingAspect}})
	return knowledgeEvidenceAssessmentJSON(verdict, missingAspect, query, message)
}

func parseKnowledgeEvidenceAssessment(args map[string]any) (verdict, missingAspect, query string, err error) {
	for key := range args {
		switch key {
		case "verdict", "missing_aspect", "external_query":
		default:
			return "", "", "", fmt.Errorf("不支持字段 %q", key)
		}
	}
	value, ok := args["verdict"].(string)
	if !ok {
		return "", "", "", fmt.Errorf("verdict 必须是字符串")
	}
	verdict = strings.ToLower(strings.TrimSpace(value))
	missingAspect, _ = args["missing_aspect"].(string)
	query, _ = args["external_query"].(string)
	missingAspect = strings.TrimSpace(missingAspect)
	query = strings.TrimSpace(query)
	switch verdict {
	case "sufficient":
		if missingAspect != "" || query != "" {
			return "", "", "", fmt.Errorf("verdict=sufficient 时 missing_aspect 和 external_query 必须为空")
		}
		return verdict, "", "", nil
	case "insufficient":
		if utf8.RuneCountInString(missingAspect) < 2 || utf8.RuneCountInString(missingAspect) > maxWebSearchMissingAspectRunes {
			return "", "", "", fmt.Errorf("missing_aspect 必须为 2..%d 个字符", maxWebSearchMissingAspectRunes)
		}
		if !webSearchQueryIsSafe(missingAspect) || !webSearchQueryIsSafe(query) {
			return "", "", "", fmt.Errorf("缺口或检索语句不得包含凭据、个人信息或空内容")
		}
		return verdict, missingAspect, query, nil
	default:
		return "", "", "", fmt.Errorf("verdict 只能是 sufficient 或 insufficient")
	}
}

func knowledgeEvidenceAssessmentJSON(status, missingAspect, query, message string) string {
	payload := map[string]any{
		"status":         status,
		"missing_aspect": missingAspect,
		"message":        message,
	}
	if query != "" {
		payload["external_search_available"] = true
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return `{"status":"invalid_request","message":"知识证据覆盖评估无法安全解析。"}`
	}
	return string(encoded)
}

// executeSearchWeb runs one configured external search after either an available
// empty KB result or a locally recorded incomplete-evidence assessment. The
// model supplies no query at this step: the outgoing text was validated and
// frozen at the earlier eligibility boundary.
func (e *Engine) executeSearchWeb(ctx context.Context, args map[string]any, onStep func(StepEvent)) string {
	if !e.webSearchMayRun() {
		message := "联网检索仅在本轮知识库已返回无足够证据后可用，且每轮最多一次。"
		onStep(StepEvent{Type: StepBlocked, Action: webSearchAction, Source: observability.ToolSourceMainReAct, Message: message})
		return webSearchResultJSON(nil, map[string]any{
			"status": "not_available", "error": message,
		})
	}
	if len(args) != 0 {
		message := "联网检索不接受参数；请使用覆盖评估已保存的检索语句。"
		onStep(StepEvent{Type: StepError, Action: webSearchAction, Source: observability.ToolSourceMainReAct, Message: message})
		return webSearchResultJSON(nil, map[string]any{
			"status": "invalid_request", "error": message,
		})
	}
	query := strings.TrimSpace(e.webSearchQueryThisTurn)
	if !webSearchQueryIsSafe(query) {
		message := "联网检索语句未通过隐私校验，不能据此补写答案。"
		onStep(StepEvent{Type: StepError, Action: webSearchAction, Source: observability.ToolSourceMainReAct, Message: message})
		return webSearchResultJSON(nil, map[string]any{
			"status": "invalid_request", "error": message,
		})
	}

	onStep(StepEvent{
		Type:   StepToolCall,
		Action: webSearchAction,
		Source: observability.ToolSourceMainReAct,
		Args:   map[string]any{"query": query},
	})
	e.webSearchCallsThisTurn++
	results, err := e.webSearcher.Search(ctx, query)
	if err != nil {
		message := "联网检索暂时不可用，不能据此补写答案。"
		onStep(StepEvent{Type: StepError, Action: webSearchAction, Source: observability.ToolSourceMainReAct, Message: message})
		return webSearchResultJSON(nil, map[string]any{
			"status": "unavailable", "error": message,
		})
	}
	message := "未找到可引用的外部资料"
	if len(results) > 0 {
		message = "已取得外部补充资料"
	}
	onStep(StepEvent{
		Type:    StepToolResult,
		Action:  webSearchAction,
		Source:  observability.ToolSourceMainReAct,
		Message: message,
		TraceResult: map[string]any{
			"sources": len(results),
		},
	})
	return webSearchResultJSON(results, map[string]any{"status": "ok"})
}

func webSearchResultJSON(results []websearch.Result, meta map[string]any) string {
	payload := map[string]any{
		"sources":              results,
		"external":             true,
		"citation_requirement": "外部资料只作补充。若采用其中的事实，必须在对应句末使用返回的 Markdown 链接；不得仅凭外部资料断定平台现行计费、配额、回收、价格、可用性或支持渠道。",
	}
	if len(results) == 0 {
		payload["empty"] = true
	}
	for key, value := range meta {
		payload[key] = value
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return `{"status":"unavailable","external":true,"error":"联网检索结果无法安全解析。"}`
	}
	return string(encoded)
}

func webSearchQueryIsSafe(query string) bool {
	query = strings.TrimSpace(query)
	if utf8.RuneCountInString(query) < 2 {
		return false
	}
	if guardrails.ContainsCredential(query) || guardrails.RedactPII(query) != query {
		return false
	}
	return true
}
