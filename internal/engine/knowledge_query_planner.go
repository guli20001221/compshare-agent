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

// The planner expands elliptical conversation into a standalone query while
// preserving exact error codes, ports, model names and commands. Both forms are
// searched because the reranker benefits from complete questions while precise
// tokens can be damaged by blanket rewriting.
const knowledgeQueryPlannerPrompt = `你是知识检索问题整理器。仅输出 JSON，不回答问题。
把当前用户问题结合必要的对话历史整理成：
{"answer_question":"用户现在真正要解决的、脱离历史也能理解的完整问题","search_queries":["用于检索的完整问题"]}
要求：
- 当前问题优先；用户已经换话题时不要继承旧主题。
- 消解“它、这个、那关机后、浏览器里呢”等指代，保留产品、环境、计费方式和限制条件。
- 不把助手过去的回答当成事实，不添加对话中没有的条件。
- search_queries 为 1 到 2 条；简单问题只写 1 条，确有多个独立方面时才拆分。
- 至少一条写成完整书面问句：补全省略的主语和对象，点明这是 GPU 云平台场景（实例、镜像、计费、存储、网络、控制台等），用平台文档里会出现的说法，不要只写关键词。
- 错误码、端口号、型号、命令名、金额等精确写法必须原样保留，不得改写、翻译或省略。`

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
		// Rewriting is a deterministic transformation, not a creative answer.
		Temperature: &knowledgeQueryPlannerTemperature,
	})
	if err != nil || resp == nil {
		return fallback
	}
	e.emitTokenUsage(resp.Usage)
	if resp.OutputIncomplete() {
		// A length-stopped (or otherwise non-normal) response can still parse as
		// valid JSON — a truncated generation is not reliably invalid JSON, it can
		// be cut off right after the closing brace — so this cannot rely on
		// parseFirstJSONObject failing below. Fall back to the Agent's own query
		// rather than risk a rewrite built on a cut-off answer_question. Usage is
		// still emitted above: the call was paid for either way.
		return fallback
	}

	var planned knowledgeQueryPlan
	if !parseFirstJSONObject(resp.Content, &planned) {
		return fallback
	}
	// Validated one slot short so the Agent's own query cannot be pushed out by a
	// full-length reply. The total stays at maxKnowledgePlanQueries, so this costs
	// no extra retrieval against maxRetrievalQueriesPerTurn.
	plan := validateKnowledgeQueryPlan(planned, fallback, maxKnowledgePlanQueries-1)
	return withAgentQueryAnchor(plan, strings.TrimSpace(proposed))
}

// withAgentQueryAnchor retains the model's precise query alongside the rewrite.
// It is appended last so the bounded retrieval budget prioritizes the standalone
// form; weak queries contribute no evidence rather than lowering other results.
func withAgentQueryAnchor(plan knowledgeQueryPlan, proposed string) knowledgeQueryPlan {
	if proposed == "" || len(plan.SearchQueries) >= maxKnowledgePlanQueries {
		return plan
	}
	key := strings.ToLower(proposed)
	for _, existing := range plan.SearchQueries {
		if strings.ToLower(strings.TrimSpace(existing)) == key {
			return plan
		}
	}
	plan.SearchQueries = append(append([]string(nil), plan.SearchQueries...), proposed)
	return plan
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
