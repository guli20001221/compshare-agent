package engine

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
)

const (
	maxKnowledgePlanQueries       = 3
	maxKnowledgePlanQuestionRunes = 600
	maxKnowledgePlanQueryRunes    = 400
)

// knowledgeQueryPlannerTemperature pins this call's sampling. It is a var only
// because ChatRequest.Temperature is a pointer (nil = provider default, so every
// other caller is untouched); nothing mutates it.
var knowledgeQueryPlannerTemperature float32 = 0

const knowledgeQueryPlannerPrompt = `你是知识检索问题整理器。仅输出 JSON，不回答问题。
把当前用户问题结合必要的对话历史整理成：
{"answer_question":"用户现在真正要解决的、脱离历史也能理解的完整问题","search_queries":["用于检索的完整问题"]}
要求：
- 当前问题优先；用户已经换话题时不要继承旧主题。
- 消解“它、这个、那关机后、浏览器里呢”等指代，保留产品、环境、计费方式和限制条件。
- 不把助手过去的回答当成事实，不添加对话中没有的条件。
- search_queries 为 1 到 3 条；简单问题只写 1 条，确有多个独立方面时才拆分。`

type knowledgeQueryPlan struct {
	AnswerQuestion string   `json:"answer_question"`
	SearchQueries  []string `json:"search_queries"`
}

type knowledgeQueryPlanInput struct {
	Conversation  []ConversationPair `json:"conversation"`
	Current       string             `json:"current_question"`
	ProposedQuery string             `json:"agent_proposed_query"`
}

// planKnowledgeQuery adds one bounded contextualization call only for a knowledge
// search that has prior conversation to resolve. It is not a second answer agent:
// the call can only return a standalone answer target plus retrieval queries.
// Any transport, parse, or validation failure falls back to the Agent-proposed
// query, so retrieval availability never depends on the planner.
func (e *Engine) planKnowledgeQuery(ctx context.Context, proposed string) knowledgeQueryPlan {
	fallback := fallbackKnowledgeQueryPlan(proposed)
	if e == nil || e.llmClient == nil || strings.TrimSpace(proposed) == "" || !e.turnContextViewReady {
		return fallback
	}
	// A first turn has no reference to resolve. Avoid spending a second model
	// call unless the Agent supplied a follow-up whose history changes the query.
	if len(e.turnContextViewThisTurn.RecentConversation) == 0 {
		return fallback
	}

	input := knowledgeQueryPlanInput{
		Conversation:  append([]ConversationPair(nil), e.turnContextViewThisTurn.RecentConversation...),
		Current:       strings.TrimSpace(e.turnContextViewThisTurn.CurrentQuestion),
		ProposedQuery: strings.TrimSpace(proposed),
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return fallback
	}
	resp, err := e.llmClient.Chat(ctx, llm.ChatRequest{
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: knowledgeQueryPlannerPrompt},
			{Role: openai.ChatMessageRoleUser, Content: string(payload)},
		},
		ResponseFormat: &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONObject,
		},
		// Pinned sampling. This call resolves references and restates one
		// question as retrieval queries — a rewrite, not a judgement, so the
		// same input should not produce a different retrieval set run to run.
		//
		// It still does. A/A over 50 real 2026-06-26..07-09 questions (same arm
		// twice, deterministic BM25 retrieval so only this call varied):
		//
		//	provider default : chunk-set flip 56%, mean Jaccard 0.708, count flip 16%
		//	pinned to 0      : chunk-set flip 50%, mean Jaccard 0.754, count flip 10%
		//
		// So the pin removes one confound and little else — the residual is not
		// sampling temperature. Anything that compares retrieval arms case by
		// case is unreadable at a 50% self-flip rate; such a comparison needs
		// repeated runs per case, or a metric that does not key on chunk-set
		// identity. Keeping the pin because a rewrite should not be sampled, NOT
		// because it made this call reproducible.
		Temperature: &knowledgeQueryPlannerTemperature,
	})
	if err != nil || resp == nil {
		return fallback
	}
	e.emitTokenUsage(resp.Usage)

	var planned knowledgeQueryPlan
	if !parseFirstJSONObject(resp.Content, &planned) {
		return fallback
	}
	return validateKnowledgeQueryPlan(planned, fallback, maxKnowledgePlanQueries)
}

func fallbackKnowledgeQueryPlan(query string) knowledgeQueryPlan {
	query = strings.TrimSpace(query)
	if query == "" {
		return knowledgeQueryPlan{}
	}
	return knowledgeQueryPlan{AnswerQuestion: query, SearchQueries: []string{query}}
}

func validateKnowledgeQueryPlan(planned, fallback knowledgeQueryPlan, maxQueries int) knowledgeQueryPlan {
	if maxQueries <= 0 {
		maxQueries = maxKnowledgePlanQueries
	}
	answer := truncateRunes(strings.TrimSpace(planned.AnswerQuestion), maxKnowledgePlanQuestionRunes)
	if answer == "" {
		answer = fallback.AnswerQuestion
	}

	queries := make([]string, 0, maxQueries)
	seen := map[string]struct{}{}
	for _, raw := range planned.SearchQueries {
		query := truncateRunes(strings.TrimSpace(raw), maxKnowledgePlanQueryRunes)
		if query == "" {
			continue
		}
		key := strings.ToLower(query)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		queries = append(queries, query)
		if len(queries) == maxQueries {
			break
		}
	}
	if len(queries) == 0 {
		queries = []string{answer}
	}
	return knowledgeQueryPlan{AnswerQuestion: answer, SearchQueries: queries}
}
