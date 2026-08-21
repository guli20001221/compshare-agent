package engine

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/compshare-agent/internal/guardrails"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/platform"
	"github.com/compshare-agent/internal/websearch"
)

const webSearchAction = "SearchWeb"

// webSearchMayRun is intentionally stricter than "a client exists". It is the
// execution-boundary counterpart to the dynamic tool-window gate: a hallucinated
// tool call cannot turn web search into a first-hop source, and a second query
// cannot spread more of a user turn across an external provider.
func (e *Engine) webSearchMayRun() bool {
	return e != nil && e.webSearcher != nil && !e.webSearchSuppressedThisTurn && e.webSearchAvailableThisTurn &&
		e.webSearchCallsThisTurn < maxWebSearchCallsPerTurn
}

// executeSearchWeb runs one configured external search after the curated KB had
// an available-but-empty result. It accepts only a whitespace/punctuation-normal
// substring of the CURRENT user message. It forbids known credentials and PII
// at the outbound boundary, and keeps model-made expansions out of the query.
func (e *Engine) executeSearchWeb(ctx context.Context, args map[string]any, onStep func(StepEvent)) string {
	if !e.webSearchMayRun() {
		message := "联网检索仅在本轮知识库已返回无足够证据后可用，且每轮最多一次。"
		onStep(StepEvent{Type: StepBlocked, Action: webSearchAction, Source: observability.ToolSourceMainReAct, Message: message})
		return webSearchResultJSON(nil, map[string]any{
			"status": "not_available", "error": message,
		})
	}
	query, ok := args["query"].(string)
	query = strings.TrimSpace(query)
	if !ok || !webSearchQueryIsCurrentUserText(query, e.lastUserMsg) {
		message := "联网检索 query 必须直接取自用户当前问题，且不得包含凭据、个人信息或模型补写内容。"
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

func webSearchQueryIsCurrentUserText(query, userMessage string) bool {
	query = strings.TrimSpace(query)
	if utf8.RuneCountInString(query) < 2 || userMessage == "" {
		return false
	}
	if guardrails.ContainsCredential(query) || guardrails.RedactPII(query) != query {
		return false
	}
	// A web query is a value sent outside the process. Its provenance must use
	// the one reviewed literal-span primitive rather than inventing another
	// text-normalization policy here. It folds case and whitespace, but does not
	// treat changed punctuation as evidence that the model copied the user.
	return platform.ContainsLiteralSpan(userMessage, query)
}
